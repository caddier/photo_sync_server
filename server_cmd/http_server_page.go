package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// createVideoFromPhotos creates a video from selected photos using ffmpeg
func createVideoFromPhotos(phoneDir string, thumbNames []string, videoName string, frameDuration float64, quality string, musicFile string) error {
	// Resolve thumbnail names to original photo paths
	var photoPaths []string
	for _, thumbName := range thumbNames {
		// Remove tbn- prefix and extension to get base name
		thumbExt := strings.ToLower(filepath.Ext(thumbName))
		base := strings.TrimSuffix(thumbName, thumbExt)
		if strings.HasPrefix(strings.ToLower(base), "tbn-") {
			base = base[4:]
		}

		// Try all possible image extensions since thumbnail extension
		// may differ from original (e.g., HEIC originals have JPG thumbnails)
		imageExts := []string{".jpg", ".jpeg", ".png", ".heic"}

		foundOriginal := false
		for _, ext := range imageExts {
			origPath := filepath.Join(phoneDir, base+ext)
			if _, err := os.Stat(origPath); err == nil {
				photoPaths = append(photoPaths, origPath)
				foundOriginal = true
				break
			}
		}

		if !foundOriginal {
			log.Printf("Warning: original file not found for thumbnail %s (base: %s)", thumbName, base)
		}
	}

	if len(photoPaths) == 0 {
		return fmt.Errorf("no valid photos found")
	}

	// Create temp directory for processing
	tempDir, err := os.MkdirTemp("", "video-creation-")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Convert HEIC files to JPEG in temp directory
	var processedPaths []string
	for i, photoPath := range photoPaths {
		ext := strings.ToLower(filepath.Ext(photoPath))

		// If it's a HEIC file, check if it's really HEIC or just a misnamed JPEG
		if ext == ".heic" {
			// Try to detect if it's actually a JPEG by checking file signature
			isActuallyJPEG := false
			if f, err := os.Open(photoPath); err == nil {
				header := make([]byte, 3)
				if n, _ := io.ReadFull(f, header); n == 3 {
					// JPEG files start with FF D8 FF
					if header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF {
						isActuallyJPEG = true
						log.Printf("File %s has .heic extension but is actually a JPEG, copying to temp folder", photoPath)
					}
				}
				f.Close()
			}

			if isActuallyJPEG {
				// Copy the JPEG file to temp directory
				jpegPath := filepath.Join(tempDir, fmt.Sprintf("copied_%d.jpg", i))
				if err := copyFile(photoPath, jpegPath); err != nil {
					log.Printf("Warning: Failed to copy JPEG file %s: %v", photoPath, err)
					continue
				}
				processedPaths = append(processedPaths, jpegPath)
				log.Printf("Copied misnamed JPEG to temp: %s -> %s", photoPath, jpegPath)
			} else {
				// It's a real HEIC file - convert to JPEG
				jpegPath := filepath.Join(tempDir, fmt.Sprintf("converted_%d.jpg", i))

				// Convert using heif-convert
				cmd := exec.Command("/usr/local/bin/heif-convert", photoPath, jpegPath)
				if output, err := cmd.CombinedOutput(); err != nil {
					log.Printf("Warning: HEIC conversion failed for %s: %v, output: %s", photoPath, err, string(output))
					continue
				}

				processedPaths = append(processedPaths, jpegPath)
				log.Printf("Converted real HEIC to JPEG for video: %s -> %s", photoPath, jpegPath)
			}
		} else {
			// Copy original file to temp directory for consistency
			tempPath := filepath.Join(tempDir, fmt.Sprintf("photo_%d%s", i, ext))
			if err := copyFile(photoPath, tempPath); err != nil {
				log.Printf("Warning: Failed to copy file %s: %v", photoPath, err)
				continue
			}
			processedPaths = append(processedPaths, tempPath)
		}
	}

	if len(processedPaths) == 0 {
		return fmt.Errorf("no valid photos after conversion")
	}

	// Create concat file for ffmpeg
	concatFile := filepath.Join(tempDir, "concat.txt")
	f, err := os.Create(concatFile)
	if err != nil {
		return fmt.Errorf("failed to create concat file: %v", err)
	}

	for _, photoPath := range processedPaths {
		// Write each photo to concat file with duration
		absPath, _ := filepath.Abs(photoPath)
		// Escape single quotes in path
		escapedPath := strings.ReplaceAll(absPath, "'", "'\\''")
		fmt.Fprintf(f, "file '%s'\n", escapedPath)
		fmt.Fprintf(f, "duration %.2f\n", frameDuration)
	}
	// Add last image again (ffmpeg concat demuxer requirement)
	if len(processedPaths) > 0 {
		absPath, _ := filepath.Abs(processedPaths[len(processedPaths)-1])
		escapedPath := strings.ReplaceAll(absPath, "'", "'\\''")
		fmt.Fprintf(f, "file '%s'\n", escapedPath)
	}
	f.Close()

	// Determine video resolution based on quality
	var scale string
	switch quality {
	case "high":
		scale = "1920:1080"
	case "medium":
		scale = "1280:720"
	case "low":
		scale = "854:480"
	default:
		scale = "1280:720"
	}

	// Output video path
	outputPath := filepath.Join(phoneDir, videoName+".mp4")
	markerPath := filepath.Join(phoneDir, "."+videoName+".created")

	// Create ffmpeg command with transition effects
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Select BGM file from /data/music
	musicDir := "/data/music"
	var bgmPath string
	useBGM := false

	if musicFile != "" {
		// Use the specified music file
		bgmPath = filepath.Join(musicDir, musicFile)
		if _, err := os.Stat(bgmPath); err == nil {
			useBGM = true
			log.Printf("Using selected background music: %s", musicFile)
		} else {
			log.Printf("Specified music file not found: %s, will use random", musicFile)
		}
	}

	// If no specific file was selected or file not found, use random
	if !useBGM {
		if musicFiles, err := os.ReadDir(musicDir); err == nil && len(musicFiles) > 0 {
			// Filter for mp3 files only
			var mp3Files []string
			for _, file := range musicFiles {
				if file.IsDir() {
					continue
				}
				ext := strings.ToLower(filepath.Ext(file.Name()))
				if ext == ".mp3" {
					mp3Files = append(mp3Files, file.Name())
				}
			}

			if len(mp3Files) > 0 {
				// Randomly select one mp3 file
				rand.Seed(time.Now().UnixNano())
				selectedFile := mp3Files[rand.Intn(len(mp3Files))]
				bgmPath = filepath.Join(musicDir, selectedFile)
				useBGM = true
				log.Printf("Selected random background music: %s", selectedFile)
			} else {
				log.Printf("No mp3 files found in %s", musicDir)
			}
		} else {
			log.Printf("Music directory %s not accessible or empty", musicDir)
		}
	}

	var args []string
	if useBGM {
		// With background music
		args = []string{
			"-f", "concat",
			"-safe", "0",
			"-i", concatFile,
			"-stream_loop", "-1", // Loop the audio
			"-i", bgmPath,
			"-vf", fmt.Sprintf("scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2,setsar=1,fade=t=in:st=0:d=0.5,fade=t=out:st=%.2f:d=0.5", scale, scale, frameDuration*float64(len(processedPaths))-0.5),
			"-c:v", "libx264",
			"-preset", "faster", // Use faster preset for speed
			"-threads", "0", // Use all available CPU cores
			"-crf", "23",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-b:a", "128k",
			"-shortest", // Stop when video ends
			"-y",
			outputPath,
		}
		log.Printf("Creating video with fade transitions and background music from %s (multi-threaded)", bgmPath)
	} else {
		// Without background music
		args = []string{
			"-f", "concat",
			"-safe", "0",
			"-i", concatFile,
			"-vf", fmt.Sprintf("scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2,setsar=1,fade=t=in:st=0:d=0.5,fade=t=out:st=%.2f:d=0.5", scale, scale, frameDuration*float64(len(processedPaths))-0.5),
			"-c:v", "libx264",
			"-preset", "faster", // Use faster preset for speed
			"-threads", "0", // Use all available CPU cores
			"-crf", "23",
			"-pix_fmt", "yuv420p",
			"-y",
			outputPath,
		}
		log.Printf("Creating video with fade transitions (no background music, multi-threaded)")
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %v, output: %s", err, string(output))
	}

	// Create marker file to indicate this video was created (not synced)
	if err := os.WriteFile(markerPath, []byte("created"), 0644); err != nil {
		log.Printf("Warning: failed to create marker file %s: %v", markerPath, err)
	}

	log.Printf("Video created successfully at %s", outputPath)
	return nil
}

// startHTTPServer starts an HTTP server with Gorilla Mux for browsing thumbnails via web browser
func startHTTPServer(config *Config) error {
	router := mux.NewRouter()

	// Home page - list all phone directories
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		entries, err := os.ReadDir(baseDir)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error reading directory: %v", err), http.StatusInternalServerError)
			return
		}

		// Define preset folders that contain files, not photos
		presetFolders := map[string]bool{
			"music": true,
			"data":  true,
		}

		var phoneDirs []string
		var fileFolders []string
		for _, e := range entries {
			if e.IsDir() {
				dirName := e.Name()
				if presetFolders[dirName] {
					fileFolders = append(fileFolders, dirName)
				} else {
					phoneDirs = append(phoneDirs, dirName)
				}
			}
		}
		sort.Strings(phoneDirs)
		sort.Strings(fileFolders)

		tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Photo Sync Server - Phone Directories</title>
    <style>
        body { font-family: 'Segoe UI', Tahoma, Arial, sans-serif; margin: 0; padding: 20px; background: #000000; color: #ffffff; }
        h1, h2 { color: #ffffff; font-weight: 300; letter-spacing: 1px; }
        h2 { font-size: 20px; margin-top: 40px; margin-bottom: 10px; color: #aaaaaa; }
        .phone-list, .file-list { list-style: none; padding: 0; max-width: 600px; }
        .phone-list li, .file-list li { margin: 15px 0; }
        .phone-list a { 
            display: block; 
            padding: 20px; 
            background: linear-gradient(135deg, #1a1a1a 0%, #2a2a2a 100%); 
            text-decoration: none; 
            color: #ffffff; 
            border-radius: 12px;
            box-shadow: 0 4px 12px rgba(0,0,0,0.5);
            transition: all 0.3s ease;
            border: 1px solid #2a2a2a;
            font-size: 16px;
        }
        .phone-list a:hover { 
            transform: translateX(10px); 
            background: linear-gradient(135deg, #2a2a2a 0%, #3a3a3a 100%); 
            border-color: #667eea;
            box-shadow: 0 6px 20px rgba(102, 126, 234, 0.4);
        }
        .file-list a {
            display: block; 
            padding: 15px 20px; 
            background: linear-gradient(135deg, #0d1a2d 0%, #1a2d45 100%); 
            text-decoration: none; 
            color: #88aaff; 
            border-radius: 8px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.4);
            transition: all 0.3s ease;
            border: 1px solid #1a3a5a;
            font-size: 14px;
        }
        .file-list a:hover {
            transform: translateX(8px); 
            background: linear-gradient(135deg, #1a2d45 0%, #2a3d55 100%); 
            border-color: #4477cc;
            box-shadow: 0 4px 16px rgba(68, 119, 204, 0.3);
            color: #aaccff;
        }
    </style>
</head>
<body>
    <h1>Photo Sync Server</h1>
    
    {{if .PhoneDirs}}
    <h2>📱 Phone Directories</h2>
    <ul class="phone-list">
        {{range .PhoneDirs}}
        <li><a href="/phone/{{.}}">📱 {{.}}</a></li>
        {{end}}
    </ul>
    {{else}}
    <p>No phone directories found.</p>
    {{end}}

    {{if .FileFolders}}
    <h2>📁 File Folders</h2>
    <ul class="file-list">
        {{range .FileFolders}}
        <li><a href="/files/{{.}}">📁 {{.}}</a></li>
        {{end}}
    </ul>
    {{end}}
</body>
</html>`

		t := template.Must(template.New("home").Parse(tmpl))
		data := struct {
			PhoneDirs   []string
			FileFolders []string
		}{
			PhoneDirs:   phoneDirs,
			FileFolders: fileFolders,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		t.Execute(w, data)
	}).Methods("GET")

	// Phone directory - show thumbnails with pagination
	router.HandleFunc("/phone/{phoneName}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		phoneName := vars["phoneName"]

		// Parse page parameter (default to 1)
		pageStr := r.URL.Query().Get("page")
		page := 1
		if pageStr != "" {
			if p, err := fmt.Sscanf(pageStr, "%d", &page); err == nil && p == 1 && page > 0 {
				// page is valid
			} else {
				page = 1
			}
		}

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		phoneDir := filepath.Join(baseDir, phoneName)
		thumbDir := filepath.Join(phoneDir, "thumbnails")

		entries, err := os.ReadDir(thumbDir)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error reading thumbnails: %v", err), http.StatusInternalServerError)
			return
		}

		var thumbFiles []string
		for _, e := range entries {
			if !e.IsDir() {
				ext := strings.ToLower(filepath.Ext(e.Name()))
				if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
					thumbName := e.Name()

					// Verify that the original file exists before adding thumbnail to list
					thumbExt := strings.ToLower(filepath.Ext(thumbName))
					base := strings.TrimSuffix(thumbName, thumbExt)
					if strings.HasPrefix(strings.ToLower(base), "tbn-") {
						base = base[4:]
					}

					// Check if original file exists with any valid extension
					imageExts := []string{".jpg", ".jpeg", ".png", ".heic"}
					videoExts := []string{".mp4", ".mov", ".m4v", ".avi", ".mkv"}
					allExts := append(imageExts, videoExts...)

					foundOriginal := false
					for _, origExt := range allExts {
						origPath := filepath.Join(phoneDir, base+origExt)
						if _, err := os.Stat(origPath); err == nil {
							foundOriginal = true
							break
						}
					}

					// Only add thumbnail if original file exists
					if foundOriginal {
						thumbFiles = append(thumbFiles, thumbName)
					} else {
						// Optional: delete orphaned thumbnail
						orphanPath := filepath.Join(thumbDir, thumbName)
						os.Remove(orphanPath)
						log.Printf("Removed orphaned thumbnail: %s (original not found)", thumbName)
					}
				}
			}
		}

		// Also include video files from the phone directory
		phoneEntries, err := os.ReadDir(phoneDir)
		if err == nil {
			for _, e := range phoneEntries {
				if !e.IsDir() {
					ext := strings.ToLower(filepath.Ext(e.Name()))
					if ext == ".mp4" {
						thumbFiles = append(thumbFiles, e.Name())
					}
				}
			}
		}
		sort.Strings(thumbFiles)

		// Pagination logic
		const itemsPerPage = 80
		totalItems := len(thumbFiles)
		totalPages := (totalItems + itemsPerPage - 1) / itemsPerPage
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages {
			page = totalPages
		}

		start := (page - 1) * itemsPerPage
		end := start + itemsPerPage
		if end > totalItems {
			end = totalItems
		}

		var pagedThumbs []string
		if start < totalItems {
			pagedThumbs = thumbFiles[start:end]
		}

		tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>{{.PhoneName}} - Thumbnails</title>
    <style>
        body { font-family: 'Segoe UI', Tahoma, Arial, sans-serif; margin: 0; padding: 20px; background: #000000; color: #ffffff; }
        h1 { color: #ffffff; font-weight: 300; letter-spacing: 1px; }
        .back-link { 
            display: inline-block; 
            margin-bottom: 20px; 
            padding: 10px 20px; 
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); 
            color: white; 
            text-decoration: none; 
            border-radius: 8px;
            transition: all 0.3s ease;
            box-shadow: 0 4px 15px rgba(102, 126, 234, 0.4);
        }
        .back-link:hover { 
            transform: translateY(-2px);
            box-shadow: 0 6px 20px rgba(102, 126, 234, 0.6);
        }
        .info-bar {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 20px;
            flex-wrap: wrap;
            gap: 10px;
        }
        .select-all-btn {
            padding: 10px 20px;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border: none;
            border-radius: 8px;
            cursor: pointer;
            font-size: 14px;
            transition: all 0.3s ease;
            font-weight: 500;
            box-shadow: 0 2px 8px rgba(102, 126, 234, 0.4);
        }
        .select-all-btn:hover {
            transform: translateY(-2px);
            box-shadow: 0 4px 12px rgba(102, 126, 234, 0.6);
        }
        .count { color: #aaaaaa; margin: 0; font-size: 14px; }
        .pagination {
            display: flex;
            gap: 5px;
            align-items: center;
        }
        .pagination a, .pagination span {
            padding: 8px 12px;
            border-radius: 6px;
            text-decoration: none;
            background: #1a1a1a;
            color: #ffffff;
            border: 1px solid #333333;
            transition: all 0.2s ease;
        }
        .pagination a:hover { 
            background: #2a2a2a;
            border-color: #667eea;
            transform: scale(1.05);
        }
        .pagination .current {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border-color: #667eea;
            box-shadow: 0 2px 10px rgba(102, 126, 234, 0.4);
        }
        .pagination .disabled {
            color: #555555;
            cursor: not-allowed;
            background: #0a0a0a;
        }
        .gallery { 
            display: grid; 
            grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); 
            gap: 20px; 
            padding: 10px;
        }
        .gallery-item { 
            background: #1a1a1a; 
            padding: 10px; 
            border-radius: 12px; 
            box-shadow: 0 4px 12px rgba(0,0,0,0.5);
            text-align: center;
            transition: all 0.3s ease;
            border: 1px solid #2a2a2a;
        }
        .gallery-item:hover {
            transform: translateY(-5px);
            box-shadow: 0 8px 20px rgba(102, 126, 234, 0.3);
            border-color: #667eea;
        }
        .gallery-item img { 
            width: 180px;
            height: 180px;
            object-fit: cover;
            border-radius: 8px;
            cursor: pointer;
            transition: all 0.3s ease;
        }
        .gallery-item img:hover { 
            opacity: 0.9;
            transform: scale(1.05);
        }
        .filename { 
            margin-top: 8px; 
            font-size: 12px; 
            color: #888888; 
            word-break: break-all;
        }
        .selection-bar {
            position: fixed;
            bottom: 20px;
            right: 20px;
            background: linear-gradient(135deg, #1a1a1a 0%, #2a2a2a 100%);
            padding: 15px 20px;
            border-radius: 12px;
            box-shadow: 0 8px 24px rgba(0,0,0,0.6);
            border: 1px solid #333333;
            display: none;
            z-index: 1000;
        }
        .selection-bar.active { display: block; }
        .selection-bar button {
            padding: 10px 20px;
            margin: 0 5px;
            border: none;
            border-radius: 8px;
            cursor: pointer;
            font-size: 14px;
            transition: all 0.3s ease;
            font-weight: 500;
        }
        .create-video-btn {
            background: linear-gradient(135deg, #4CAF50 0%, #45a049 100%);
            color: white;
            box-shadow: 0 2px 8px rgba(76, 175, 80, 0.4);
        }
        .create-video-btn:hover { 
            transform: translateY(-2px);
            box-shadow: 0 4px 12px rgba(76, 175, 80, 0.6);
        }
        .clear-selection-btn {
            background: linear-gradient(135deg, #f44336 0%, #da190b 100%);
            color: white;
            box-shadow: 0 2px 8px rgba(244, 67, 54, 0.4);
        }
        .clear-selection-btn:hover { 
            transform: translateY(-2px);
            box-shadow: 0 4px 12px rgba(244, 67, 54, 0.6);
        }
        .gallery-item.selected {
            border: 3px solid #667eea;
            box-shadow: 0 8px 20px rgba(102, 126, 234, 0.5);
        }
        .gallery-item .checkbox {
            position: relative;
            display: block;
            margin: 5px auto 0 auto;
            width: 24px;
            height: 24px;
            cursor: pointer;
            z-index: 10;
            pointer-events: auto;
        }
        .gallery-item {
            position: relative;
        }
        #videoModal {
            display: none;
            position: fixed;
            z-index: 2000;
            left: 0;
            top: 0;
            width: 100%;
            height: 100%;
            background: rgba(0,0,0,0.85);
        }
        #videoModal .modal-content {
            background: #1a1a1a;
            margin: 10% auto;
            padding: 30px;
            width: 500px;
            border-radius: 12px;
            border: 1px solid #333333;
            box-shadow: 0 10px 40px rgba(0,0,0,0.8);
        }
        #videoModal h2 { margin-top: 0; color: #ffffff; }
        #videoModal input, #videoModal select {
            width: 100%;
            padding: 10px;
            margin: 10px 0;
            border: 1px solid #333333;
            border-radius: 8px;
            box-sizing: border-box;
            background: #0a0a0a;
            color: #ffffff;
            transition: border-color 0.3s ease;
        }
        #videoModal input:focus, #videoModal select:focus {
            outline: none;
            border-color: #667eea;
            box-shadow: 0 0 0 3px rgba(102, 126, 234, 0.1);
        }
        #videoModal button {
            padding: 10px 20px;
            margin: 10px 5px 0 0;
            border: none;
            border-radius: 8px;
            cursor: pointer;
            transition: all 0.3s ease;
            font-weight: 500;
        }
        .modal-create { 
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); 
            color: white; 
            box-shadow: 0 4px 12px rgba(102, 126, 234, 0.4);
        }
        .modal-create:hover { 
            transform: translateY(-2px);
            box-shadow: 0 6px 16px rgba(102, 126, 234, 0.6);
        }
        .modal-cancel { 
            background: #2a2a2a; 
            color: white; 
            border: 1px solid #333333;
        }
        .modal-cancel:hover { background: #3a3a3a; }
        #videoStatus {
            margin-top: 15px;
            padding: 10px;
            border-radius: 8px;
            display: none;
        }
        #videoStatus.success { background: #1a3a1a; color: #4ade80; border: 1px solid #2a4a2a; }
        #videoStatus.error { background: #3a1a1a; color: #f87171; border: 1px solid #4a2a2a; }
        #videoStatus.info { background: #1a2a3a; color: #60a5fa; border: 1px solid #2a3a4a; }
        
        /* Video player modal */
        #videoPlayerModal {
            display: none;
            position: fixed;
            z-index: 3000;
            left: 0;
            top: 0;
            width: 100%;
            height: 100%;
            background-color: rgba(0,0,0,0.9);
        }
        #videoPlayerModal .modal-content {
            position: relative;
            margin: 5% auto;
            width: 80%;
            max-width: 1200px;
        }
        #videoPlayerModal video {
            width: 100%;
            max-height: 80vh;
            background: #000;
        }
        #videoPlayerModal .close {
            position: absolute;
            top: -40px;
            right: 0;
            color: #f1f1f1;
            font-size: 40px;
            font-weight: bold;
            cursor: pointer;
        }
        #videoPlayerModal .close:hover { color: #bbb; }
        
        /* Photo viewer modal */
        #photoViewerModal {
            display: none;
            position: fixed;
            z-index: 3000;
            left: 0;
            top: 0;
            width: 100%;
            height: 100%;
            background-color: rgba(0,0,0,0.95);
            overflow: auto;
        }
        #photoViewerModal .modal-content {
            position: relative;
            margin: 2% auto;
            width: 90%;
            max-width: 1400px;
            text-align: center;
        }
        #photoViewerModal img {
            max-width: 100%;
            max-height: 90vh;
            object-fit: contain;
            border-radius: 5px;
        }
        #photoViewerModal .close {
            position: absolute;
            top: 10px;
            right: 25px;
            color: #f1f1f1;
            font-size: 40px;
            font-weight: bold;
            cursor: pointer;
            z-index: 3001;
        }
        #photoViewerModal .close:hover { color: #bbb; }
        #photoViewerModal .photo-filename {
            color: #f1f1f1;
            margin-top: 15px;
            font-size: 16px;
        }
        
        /* YouTube download section */
        .youtube-download {
            background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%);
            padding: 20px;
            margin: 20px 0;
            border-radius: 12px;
            border: 1px solid #333;
            box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
        }
        .youtube-download h3 {
            margin: 0 0 15px 0;
            color: #ffffff;
            font-size: 18px;
            display: flex;
            align-items: center;
            gap: 10px;
        }
        .youtube-input-group {
            display: flex;
            gap: 10px;
            align-items: center;
        }
        .youtube-input-group input {
            flex: 1;
            padding: 12px 15px;
            border: 1px solid #333;
            border-radius: 8px;
            background: #0a0a0a;
            color: #ffffff;
            font-size: 14px;
            transition: all 0.3s ease;
        }
        .youtube-input-group input:focus {
            outline: none;
            border-color: #667eea;
            box-shadow: 0 0 0 3px rgba(102, 126, 234, 0.1);
        }
        .youtube-input-group button {
            padding: 12px 24px;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border: none;
            border-radius: 8px;
            cursor: pointer;
            font-weight: 500;
            transition: all 0.3s ease;
            box-shadow: 0 4px 12px rgba(102, 126, 234, 0.4);
        }
        .youtube-input-group button:hover {
            transform: translateY(-2px);
            box-shadow: 0 6px 16px rgba(102, 126, 234, 0.6);
        }
        .youtube-input-group button:disabled {
            opacity: 0.5;
            cursor: not-allowed;
            transform: none;
        }
        #downloadStatus {
            margin-top: 15px;
            padding: 12px;
            border-radius: 8px;
            display: none;
            font-size: 14px;
        }
        #downloadStatus.success { background: #1a3a1a; color: #4ade80; border: 1px solid #2a4a2a; }
        #downloadStatus.error { background: #3a1a1a; color: #f87171; border: 1px solid #4a2a2a; }
        #downloadStatus.info { background: #1a2a3a; color: #60a5fa; border: 1px solid #2a3a4a; }

        /* Video badge in gallery */
        .video-badge {
            position: absolute;
            top: 15px;
            right: 15px;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 5px 10px;
            border-radius: 6px;
            font-size: 12px;
            z-index: 5;
            font-weight: 500;
            box-shadow: 0 2px 8px rgba(102, 126, 234, 0.4);
        }
        .gallery-item.video-item img {
            opacity: 0.9;
        }
        .gallery-item.video-item::after {
            content: '▶';
            position: absolute;
            top: 50%;
            left: 50%;
            transform: translate(-50%, -50%);
            font-size: 48px;
            color: rgba(255,255,255,0.8);
            pointer-events: none;
            z-index: 5;
        }
    </style>
</head>
<body>
    <a href="/" class="back-link">← Back to Phone List</a>
    <h1>📱 {{.PhoneName}}</h1>
    
    <div class="youtube-download">
        <h3>🎵 Download Music from YouTube</h3>
        <div class="youtube-input-group">
            <input type="text" id="youtubeUrl" placeholder="Enter YouTube video URL..." />
            <button onclick="downloadMusic()" id="downloadBtn">Download</button>
        </div>
        <div id="downloadStatus"></div>
    </div>

    <div class="info-bar">
        <p class="count">Total: {{.TotalItems}} | Page {{.CurrentPage}} of {{.TotalPages}}</p>
        <button class="select-all-btn" onclick="selectAllOnPage()">✓ Select All on Page</button>
        <div class="pagination">
            {{if gt .CurrentPage 1}}
                <a href="?page=1">« First</a>
                <a href="?page={{.PrevPage}}">‹ Prev</a>
            {{else}}
                <span class="disabled">« First</span>
                <span class="disabled">‹ Prev</span>
            {{end}}
            
            {{range .PageNumbers}}
                {{if eq . $.CurrentPage}}
                    <span class="current">{{.}}</span>
                {{else}}
                    <a href="?page={{.}}">{{.}}</a>
                {{end}}
            {{end}}
            
            {{if lt .CurrentPage .TotalPages}}
                <a href="?page={{.NextPage}}">Next ›</a>
                <a href="?page={{.TotalPages}}">Last »</a>
            {{else}}
                <span class="disabled">Next ›</span>
                <span class="disabled">Last »</span>
            {{end}}
        </div>
    </div>
    {{if .Thumbs}}
    <div class="gallery">
        {{range .Thumbs}}
        {{if hasSuffix . ".mp4"}}
		<div class="gallery-item video-item" data-filename="{{.}}" data-is-video="true">
            <span class="video-badge">🎬 VIDEO</span>
			<a href="#" onclick="playVideo('{{$.PhoneName}}', '{{.}}'); return false;">
				<img src="/thumb/{{$.PhoneName}}/tbn-{{.}}" alt="{{.}}" onerror="this.src='data:image/svg+xml,%3Csvg xmlns=%22http://www.w3.org/2000/svg%22 width=%22200%22 height=%22200%22%3E%3Crect fill=%22%23333%22 width=%22200%22 height=%22200%22/%3E%3Ctext fill=%22%23fff%22 x=%2250%25%22 y=%2250%25%22 text-anchor=%22middle%22 dy=%22.3em%22%3EVIDEO%3C/text%3E%3C/svg%3E'" />
			</a>
            <div class="filename">{{.}}</div>
        </div>
        {{else}}
		<div class="gallery-item" data-filename="{{.}}">
			<a href="#" onclick="viewPhoto('{{$.PhoneName}}', '{{.}}'); return false;">
				<img src="/thumb/{{$.PhoneName}}/{{.}}" alt="{{.}}" />
			</a>
            <div class="filename">{{.}}</div>
            <input type="checkbox" class="checkbox" data-filename="{{.}}">
        </div>
        {{end}}
        {{end}}
    </div>
    {{else}}
    <p>No thumbnails found.</p>
    {{end}}
    
    <div class="selection-bar" id="selectionBar">
        <span id="selectionCount">0 selected</span>
        <button class="create-video-btn" onclick="showVideoModal()">🎬 Create Video</button>
        <button class="clear-selection-btn" onclick="clearSelection()">✕ Clear</button>
    </div>

    <div id="videoModal">
        <div class="modal-content">
            <h2>Create Video from Photos</h2>
            <label>Video Name:</label>
            <input type="text" id="videoName" placeholder="my_video" value="slideshow">
            
            <label>Frame Duration (seconds per photo):</label>
            <input type="number" id="frameDuration" value="2" min="0.5" max="10" step="0.5">
            
            <label>Video Quality:</label>
            <select id="videoQuality">
                <option value="high">High (1080p)</option>
                <option value="medium" selected>Medium (720p)</option>
                <option value="low">Low (480p)</option>
            </select>
            
            <label>Background Music:</label>
            <select id="musicFile">
                <option value="">Random Music</option>
                {{range .MusicFiles}}
                <option value="{{.}}">{{.}}</option>
                {{end}}
            </select>
            
            <div>
                <button class="modal-create" onclick="createVideo()">Create Video</button>
                <button class="modal-cancel" onclick="closeVideoModal()">Cancel</button>
            </div>
            
            <div id="videoStatus"></div>
        </div>
    </div>

    <div id="videoPlayerModal">
        <div class="modal-content">
            <span class="close" onclick="closeVideoPlayer()">&times;</span>
            <video id="videoPlayer" controls autoplay>
                <source id="videoSource" src="" type="video/mp4">
                Your browser does not support the video tag.
            </video>
        </div>
    </div>

    <div id="photoViewerModal">
        <div class="modal-content">
            <span class="close" onclick="closePhotoViewer()">&times;</span>
            <img id="photoViewerImg" src="" alt="Photo">
            <div class="photo-filename" id="photoFilename"></div>
        </div>
    </div>

    <script>
        let selectedPhotos = new Set();
        const phoneName = '{{.PhoneName}}';

        document.querySelectorAll('.checkbox').forEach(cb => {
            cb.addEventListener('change', function(e) {
                e.stopPropagation();
                const filename = this.dataset.filename;
                const item = this.closest('.gallery-item');
                
                if (this.checked) {
                    selectedPhotos.add(filename);
                    item.classList.add('selected');
                } else {
                    selectedPhotos.delete(filename);
                    item.classList.remove('selected');
                }
                
                updateSelectionBar();
            });
            
            // Prevent checkbox clicks from triggering the link
            cb.addEventListener('click', function(e) {
                e.stopPropagation();
            });
        });
        
        // Prevent clicks on the checkbox area from opening the image
        document.querySelectorAll('.gallery-item').forEach(item => {
            item.addEventListener('click', function(e) {
                if (e.target.classList.contains('checkbox') || e.target.closest('.checkbox')) {
                    e.preventDefault();
                    e.stopPropagation();
                }
            });
        });

        function updateSelectionBar() {
            const count = selectedPhotos.size;
            document.getElementById('selectionCount').textContent = count + ' selected';
            document.getElementById('selectionBar').classList.toggle('active', count > 0);
        }

        function clearSelection() {
            selectedPhotos.clear();
            document.querySelectorAll('.checkbox').forEach(cb => {
                cb.checked = false;
            });
            document.querySelectorAll('.gallery-item').forEach(item => {
                item.classList.remove('selected');
            });
            updateSelectionBar();
        }

        function selectAllOnPage() {
            document.querySelectorAll('.checkbox').forEach(cb => {
                const filename = cb.dataset.filename;
                cb.checked = true;
                selectedPhotos.add(filename);
                cb.closest('.gallery-item').classList.add('selected');
            });
            updateSelectionBar();
        }

        function downloadMusic() {
            const urlInput = document.getElementById('youtubeUrl');
            const url = urlInput.value.trim();
            const statusDiv = document.getElementById('downloadStatus');
            const downloadBtn = document.getElementById('downloadBtn');
            
            if (!url) {
                statusDiv.className = 'error';
                statusDiv.textContent = 'Please enter a YouTube URL';
                statusDiv.style.display = 'block';
                return;
            }
            
            downloadBtn.disabled = true;
            downloadBtn.textContent = 'Downloading...';
            statusDiv.className = 'info';
            statusDiv.textContent = 'Downloading music from YouTube...';
            statusDiv.style.display = 'block';
            
            fetch('/download-music', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ url: url })
            })
            .then(response => response.json())
            .then(data => {
                if (data.success) {
                    statusDiv.className = 'success';
                    statusDiv.textContent = '✓ ' + data.message;
                    urlInput.value = '';
                } else {
                    statusDiv.className = 'error';
                    statusDiv.textContent = '✗ ' + (data.error || 'Download failed');
                }
            })
            .catch(error => {
                statusDiv.className = 'error';
                statusDiv.textContent = '✗ Error: ' + error.message;
            })
            .finally(() => {
                downloadBtn.disabled = false;
                downloadBtn.textContent = 'Download';
            });
        }

        function showVideoModal() {
            if (selectedPhotos.size === 0) {
                alert('Please select at least one photo');
                return;
            }
            document.getElementById('videoModal').style.display = 'block';
            document.getElementById('videoStatus').style.display = 'none';
        }

        function closeVideoModal() {
            document.getElementById('videoModal').style.display = 'none';
        }

        function createVideo() {
            const videoName = document.getElementById('videoName').value || 'slideshow';
            const frameDuration = parseFloat(document.getElementById('frameDuration').value);
            const videoQuality = document.getElementById('videoQuality').value;
            const musicFile = document.getElementById('musicFile').value;
            
            if (selectedPhotos.size === 0) {
                alert('No photos selected');
                return;
            }

            const status = document.getElementById('videoStatus');
            status.className = 'info';
            status.style.display = 'block';
            status.textContent = 'Creating video... This may take a few minutes.';

            const payload = {
                phoneName: phoneName,
                photos: Array.from(selectedPhotos),
                videoName: videoName,
                frameDuration: frameDuration,
                quality: videoQuality,
                musicFile: musicFile
            };

            fetch('/create-video', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            })
            .then(response => response.json())
            .then(data => {
                if (data.success) {
                    status.className = 'success';
                    status.textContent = 'Video created successfully! Opening video player...';
                    
                    // Video is ready now (synchronous creation)
                    closeVideoModal();
                    clearSelection();
                    
                    // Play the newly created video
                    playVideo(phoneName, data.filename, true);
                } else {
                    status.className = 'error';
                    status.textContent = 'Error: ' + (data.error || 'Unknown error');
                }
            })
            .catch(err => {
                status.className = 'error';
                status.textContent = 'Error: ' + err.message;
            });
        }

        let shouldReloadAfterVideo = false;

        function playVideo(phone, filename, reloadAfterClose) {
            const videoSource = document.getElementById('videoSource');
            const videoPlayer = document.getElementById('videoPlayer');
            const videoUrl = '/orig/' + phone + '/' + filename;
            
            shouldReloadAfterVideo = reloadAfterClose || false;
            
            console.log('Playing video:', videoUrl);
            videoSource.src = videoUrl;
            videoPlayer.load();
            
            videoPlayer.onerror = function(e) {
                console.error('Video load error:', e);
                alert('Failed to load video: ' + filename + '\nURL: ' + videoUrl);
            };
            
            document.getElementById('videoPlayerModal').style.display = 'block';
        }

        function closeVideoPlayer() {
            const videoPlayer = document.getElementById('videoPlayer');
            videoPlayer.pause();
            videoPlayer.currentTime = 0;
            document.getElementById('videoPlayerModal').style.display = 'none';
            
            // Reload page if this was a newly created video
            if (shouldReloadAfterVideo) {
                shouldReloadAfterVideo = false;
                window.location.reload();
            }
        }

        function viewPhoto(phone, filename) {
            const photoImg = document.getElementById('photoViewerImg');
            const photoFilename = document.getElementById('photoFilename');
            const photoUrl = '/orig/' + phone + '/' + filename;
            
            console.log('Viewing photo:', photoUrl);
            photoImg.src = photoUrl;
            photoFilename.textContent = filename;
            
            photoImg.onerror = function(e) {
                console.error('Photo load error:', e);
                alert('Failed to load photo: ' + filename + '\nURL: ' + photoUrl);
            };
            
            document.getElementById('photoViewerModal').style.display = 'block';
        }

        function closePhotoViewer() {
            document.getElementById('photoViewerModal').style.display = 'none';
        }

        // Close modal when clicking outside
        window.onclick = function(event) {
            const modal = document.getElementById('videoModal');
            const videoModal = document.getElementById('videoPlayerModal');
            const photoModal = document.getElementById('photoViewerModal');
            if (event.target == modal) {
                closeVideoModal();
            }
            if (event.target == videoModal) {
                closeVideoPlayer();
            }
            if (event.target == photoModal) {
                closePhotoViewer();
            }
        }
    </script>
</body>
</html>`

		// Generate page numbers for pagination (show max 7 page links)
		var pageNumbers []int
		maxLinks := 7
		startPage := page - maxLinks/2
		if startPage < 1 {
			startPage = 1
		}
		endPage := startPage + maxLinks - 1
		if endPage > totalPages {
			endPage = totalPages
			startPage = endPage - maxLinks + 1
			if startPage < 1 {
				startPage = 1
			}
		}
		for i := startPage; i <= endPage; i++ {
			pageNumbers = append(pageNumbers, i)
		}

		// Get music files from /data/music
		musicDir := "/data/music"
		var musicFiles []string
		if musicEntries, err := os.ReadDir(musicDir); err == nil {
			for _, entry := range musicEntries {
				if !entry.IsDir() {
					ext := strings.ToLower(filepath.Ext(entry.Name()))
					if ext == ".mp3" {
						musicFiles = append(musicFiles, entry.Name())
					}
				}
			}
		}
		sort.Strings(musicFiles)

		t := template.Must(template.New("phone").Funcs(template.FuncMap{
			"hasSuffix": strings.HasSuffix,
		}).Parse(tmpl))
		data := struct {
			PhoneName   string
			Thumbs      []string
			TotalItems  int
			TotalPages  int
			CurrentPage int
			PrevPage    int
			NextPage    int
			PageNumbers []int
			MusicFiles  []string
		}{
			PhoneName:   phoneName,
			Thumbs:      pagedThumbs,
			TotalItems:  totalItems,
			TotalPages:  totalPages,
			CurrentPage: page,
			PrevPage:    page - 1,
			NextPage:    page + 1,
			PageNumbers: pageNumbers,
			MusicFiles:  musicFiles,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		t.Execute(w, data)
	}).Methods("GET")

	// Serve thumbnail images
	router.HandleFunc("/thumb/{phoneName}/{fileName}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		phoneName := vars["phoneName"]
		fileName := vars["fileName"]

		// Security: prevent path traversal
		if strings.Contains(phoneName, "..") || strings.Contains(fileName, "..") {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		filePath := filepath.Join(baseDir, phoneName, "thumbnails", fileName)

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}

		http.ServeFile(w, r, filePath)
	}).Methods("GET")

	// Serve original media corresponding to a thumbnail name
	router.HandleFunc("/orig/{phoneName}/{thumbName}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		phoneName := vars["phoneName"]
		thumbName := vars["thumbName"]

		// Security: prevent path traversal
		if strings.Contains(phoneName, "..") || strings.Contains(thumbName, "..") {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		phoneDir := filepath.Join(baseDir, phoneName)

		// If thumbName is a direct video file (e.g., .mp4), serve it directly
		if strings.ToLower(filepath.Ext(thumbName)) == ".mp4" {
			videoPath := filepath.Join(phoneDir, thumbName)
			if _, err := os.Stat(videoPath); err == nil {
				w.Header().Set("Content-Type", "video/mp4")
				http.ServeFile(w, r, videoPath)
				return
			}
		}

		// Derive base name from thumbnail: remove extension and optional tbn- prefix
		thumbExt := strings.ToLower(filepath.Ext(thumbName))
		base := strings.TrimSuffix(thumbName, thumbExt)
		if strings.HasPrefix(strings.ToLower(base), "tbn-") {
			base = base[4:]
		}

		log.Printf("Looking for original: thumbName=%s, base=%s, phoneDir=%s", thumbName, base, phoneDir)

		// Try all possible image and video extensions since thumbnail extension
		// may differ from original (e.g., HEIC originals have JPG thumbnails)
		imageExts := []string{".jpg", ".jpeg", ".png", ".heic"}
		videoExts := []string{".mp4", ".mov", ".m4v", ".avi", ".mkv"}

		// First try images
		for _, ext := range imageExts {
			orig := filepath.Join(phoneDir, base+ext)
			if _, err := os.Stat(orig); err == nil {
				log.Printf("Found original image: %s", orig)

				// If it's a HEIC file, check if it's really HEIC or just a misnamed JPEG
				if strings.ToLower(ext) == ".heic" {
					// Try to detect if it's actually a JPEG by checking file signature
					isActuallyJPEG := false
					if f, err := os.Open(orig); err == nil {
						header := make([]byte, 3)
						if n, _ := io.ReadFull(f, header); n == 3 {
							// JPEG files start with FF D8 FF
							if header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF {
								isActuallyJPEG = true
								log.Printf("File %s has .heic extension but is actually a JPEG", orig)
							}
						}
						f.Close()
					}

					if isActuallyJPEG {
						// Just serve it as JPEG
						w.Header().Set("Content-Type", "image/jpeg")
						http.ServeFile(w, r, orig)
						return
					}

					// It's a real HEIC file - convert to JPEG for browser compatibility
					log.Printf("Converting real HEIC to JPEG for browser: %s", orig)

					// Create temporary JPEG file
					tmpFile, err := os.CreateTemp("", "heic-web-*.jpg")
					if err != nil {
						log.Printf("Error creating temp file for HEIC conversion: %v", err)
						http.Error(w, "Error processing image", http.StatusInternalServerError)
						return
					}
					tmpPath := tmpFile.Name()
					tmpFile.Close()
					defer os.Remove(tmpPath)

					// Convert using heif-convert
					cmd := exec.Command("/usr/local/bin/heif-convert", orig, tmpPath)
					if output, err := cmd.CombinedOutput(); err != nil {
						log.Printf("HEIC conversion failed: %v, output: %s", err, string(output))
						http.Error(w, "Error converting image", http.StatusInternalServerError)
						return
					}

					// Serve the converted JPEG
					w.Header().Set("Content-Type", "image/jpeg")
					http.ServeFile(w, r, tmpPath)
					return
				}

				http.ServeFile(w, r, orig)
				return
			}
		}

		// Then try videos (common formats)
		for _, ext := range videoExts {
			orig := filepath.Join(phoneDir, base+ext)
			if _, err := os.Stat(orig); err == nil {
				log.Printf("Found original video: %s", orig)
				http.ServeFile(w, r, orig)
				return
			}
		}

		log.Printf("Original file not found: thumbName=%s, base=%s", thumbName, base)
		http.NotFound(w, r)
	}).Methods("GET")

	// Create video from selected photos
	router.HandleFunc("/download-music", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			URL string `json:"url"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Invalid request format",
			})
			return
		}

		if req.URL == "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "URL is required",
			})
			return
		}

		// Determine the next bgm filename
		musicDir := "/data/music"
		files, err := os.ReadDir(musicDir)
		if err != nil {
			// If directory doesn't exist, create it
			if os.IsNotExist(err) {
				if err := os.MkdirAll(musicDir, 0755); err != nil {
					log.Printf("Failed to create music directory: %v", err)
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"success": false,
						"error":   "Failed to create music directory",
					})
					return
				}
				files = []os.DirEntry{}
			} else {
				log.Printf("Failed to read music directory: %v", err)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"error":   "Failed to access music directory",
				})
				return
			}
		}

		// Count existing bgm*.mp3 files
		bgmCount := 0
		for _, file := range files {
			if !file.IsDir() && strings.HasPrefix(file.Name(), "bgm") && strings.HasSuffix(file.Name(), ".mp3") {
				bgmCount++
			}
		}

		// Next file will be bgm(N+1).mp3
		nextNum := bgmCount + 1
		fileName := fmt.Sprintf("bgm%d", nextNum)

		log.Printf("Downloading music from %s as %s.mp3", req.URL, fileName)

		// Execute music_get_linux command
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(ctx, "/usr/local/bin/music_get_linux",
			"-output", musicDir,
			"-name", fileName,
			"-url", req.URL)

		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("Failed to download music: %v\nOutput: %s", err, string(output))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Download failed: %v", err),
			})
			return
		}

		log.Printf("Music downloaded successfully: %s.mp3", fileName)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"message":  fmt.Sprintf("Music downloaded successfully as %s.mp3", fileName),
			"filename": fileName + ".mp3",
		})
	}).Methods("POST")

	router.HandleFunc("/create-video", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			PhoneName     string   `json:"phoneName"`
			Photos        []string `json:"photos"`
			VideoName     string   `json:"videoName"`
			FrameDuration float64  `json:"frameDuration"`
			Quality       string   `json:"quality"`
			MusicFile     string   `json:"musicFile"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Invalid request: " + err.Error(),
			})
			return
		}

		if len(req.Photos) == 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "No photos selected",
			})
			return
		}

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		phoneDir := filepath.Join(baseDir, req.PhoneName)
		videoName := req.VideoName
		if videoName == "" {
			videoName = "slideshow"
		}

		// Create video synchronously so it's ready before we respond
		if err := createVideoFromPhotos(phoneDir, req.Photos, videoName, req.FrameDuration, req.Quality, req.MusicFile); err != nil {
			log.Printf("Error creating video: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Video creation failed: %v", err),
			})
			return
		}

		log.Printf("Video created successfully: %s.mp4", videoName)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"filename": videoName + ".mp4",
			"message":  "Video created successfully",
		})
	}).Methods("POST")

	// File folder viewer - list files in preset folders (music, data, etc.)
	router.HandleFunc("/files/{folderName}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		folderName := vars["folderName"]

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		folderPath := filepath.Join(baseDir, folderName)

		// Security check - ensure folder exists and is a directory
		info, err := os.Stat(folderPath)
		if err != nil || !info.IsDir() {
			http.Error(w, "Folder not found", http.StatusNotFound)
			return
		}

		// Read files in the folder
		entries, err := os.ReadDir(folderPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error reading folder: %v", err), http.StatusInternalServerError)
			return
		}

		type FileInfo struct {
			Name      string
			Size      int64
			IsDir     bool
			Extension string
		}

		var files []FileInfo
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}

			ext := strings.ToLower(filepath.Ext(e.Name()))
			files = append(files, FileInfo{
				Name:      e.Name(),
				Size:      info.Size(),
				IsDir:     e.IsDir(),
				Extension: ext,
			})
		}

		// Sort: directories first, then by name
		sort.Slice(files, func(i, j int) bool {
			if files[i].IsDir != files[j].IsDir {
				return files[i].IsDir
			}
			return files[i].Name < files[j].Name
		})

		tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>{{.FolderName}} - File Browser</title>
    <style>
        body { font-family: 'Segoe UI', Tahoma, Arial, sans-serif; margin: 0; padding: 20px; background: #000000; color: #ffffff; }
        h1 { color: #ffffff; font-weight: 300; letter-spacing: 1px; }
        .back-link { 
            display: inline-block; 
            margin-bottom: 20px; 
            color: #88aaff; 
            text-decoration: none; 
            font-size: 14px;
        }
        .back-link:hover { color: #aaccff; text-decoration: underline; }
        .file-list { list-style: none; padding: 0; max-width: 800px; }
        .file-list li { margin: 10px 0; }
        .file-item { 
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 15px 20px; 
            background: linear-gradient(135deg, #1a1a1a 0%, #2a2a2a 100%); 
            border-radius: 8px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.4);
            border: 1px solid #2a2a2a;
            transition: all 0.3s ease;
        }
        .file-item:hover {
            background: linear-gradient(135deg, #2a2a2a 0%, #3a3a3a 100%); 
            border-color: #667eea;
            transform: translateX(5px);
        }
        .file-name { 
            color: #ffffff; 
            text-decoration: none;
            flex-grow: 1;
            display: flex;
            align-items: center;
        }
        .file-name:hover { color: #aaccff; }
        .file-icon { margin-right: 10px; font-size: 18px; }
        .file-size { color: #888888; font-size: 12px; margin-left: 20px; }
        .download-btn {
            padding: 6px 12px;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            border: none;
            border-radius: 6px;
            cursor: pointer;
            text-decoration: none;
            font-size: 12px;
            transition: all 0.3s ease;
        }
        .download-btn:hover {
            transform: scale(1.05);
            box-shadow: 0 4px 12px rgba(102, 126, 234, 0.5);
        }
        .folder-item .file-name { color: #88aaff; }
        .empty-message { color: #888888; padding: 20px; }
    </style>
</head>
<body>
    <a href="/" class="back-link">← Back to Home</a>
    <h1>📁 {{.FolderName}}</h1>
    
    {{if .Files}}
    <ul class="file-list">
        {{range .Files}}
        <li>
            <div class="file-item {{if .IsDir}}folder-item{{end}}">
                <span class="file-name">
                    <span class="file-icon">{{if .IsDir}}📁{{else}}📄{{end}}</span>
                    {{.Name}}
                    {{if not .IsDir}}
                    <span class="file-size">({{.Size}} bytes)</span>
                    {{end}}
                </span>
                {{if not .IsDir}}
                <a href="/download/{{$.FolderName}}/{{.Name}}" class="download-btn" download>Download</a>
                {{end}}
            </div>
        </li>
        {{end}}
    </ul>
    {{else}}
    <p class="empty-message">This folder is empty.</p>
    {{end}}
</body>
</html>`

		t := template.Must(template.New("files").Parse(tmpl))
		data := struct {
			FolderName string
			Files      []FileInfo
		}{
			FolderName: folderName,
			Files:      files,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		t.Execute(w, data)
	}).Methods("GET")

	// Download handler for files in preset folders
	router.HandleFunc("/download/{folderName}/{fileName}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		folderName := vars["folderName"]
		fileName := vars["fileName"]

		baseDir := config.ReceiveDir
		if baseDir == "" {
			baseDir = "received"
		}

		filePath := filepath.Join(baseDir, folderName, fileName)

		// Security check - ensure path doesn't escape base directory
		absBase, _ := filepath.Abs(baseDir)
		absFile, _ := filepath.Abs(filePath)
		if !strings.HasPrefix(absFile, absBase) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}

		// Check if file exists and is not a directory
		info, err := os.Stat(filePath)
		if err != nil || info.IsDir() {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// Serve the file
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
		http.ServeFile(w, r, filePath)
	}).Methods("GET")

	port := config.HttpPort
	if port == "" {
		port = ":8080"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	log.Printf("HTTP Server listening on port %s\n", port)
	return http.ListenAndServe(port, router)
}
