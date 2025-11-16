package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
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
func createVideoFromPhotos(phoneDir string, thumbNames []string, videoName string, frameDuration float64, quality string) error {
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

		// If it's a HEIC file, convert to JPEG
		if ext == ".heic" {
			jpegPath := filepath.Join(tempDir, fmt.Sprintf("converted_%d.jpg", i))

			// Convert using heif-convert
			cmd := exec.Command("/usr/local/bin/heif-convert", photoPath, jpegPath)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("Warning: HEIC conversion failed for %s: %v, output: %s", photoPath, err, string(output))
				continue
			}

			processedPaths = append(processedPaths, jpegPath)
			log.Printf("Converted HEIC to JPEG for video: %s -> %s", photoPath, jpegPath)
		} else {
			// Use original file if it's already JPEG/PNG
			processedPaths = append(processedPaths, photoPath)
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

	// Create ffmpeg command
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Select a random BGM file from /data/music
	musicDir := "/data/music"
	var bgmPath string
	useBGM := false

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
			log.Printf("Selected background music: %s", selectedFile)
		} else {
			log.Printf("No mp3 files found in %s", musicDir)
		}
	} else {
		log.Printf("Music directory %s not accessible or empty", musicDir)
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
			"-vf", fmt.Sprintf("scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2,setsar=1", scale, scale),
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", "23",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-b:a", "128k",
			"-shortest", // Stop when video ends
			"-y",
			outputPath,
		}
		log.Printf("Creating video with background music from %s", bgmPath)
	} else {
		// Without background music (original code)
		args = []string{
			"-f", "concat",
			"-safe", "0",
			"-i", concatFile,
			"-vf", fmt.Sprintf("scale=%s:force_original_aspect_ratio=decrease,pad=%s:(ow-iw)/2:(oh-ih)/2,setsar=1", scale, scale),
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", "23",
			"-pix_fmt", "yuv420p",
			"-y",
			outputPath,
		}
		log.Printf("Creating video without background music (no music files available)")
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

		var phoneDirs []string
		for _, e := range entries {
			if e.IsDir() {
				phoneDirs = append(phoneDirs, e.Name())
			}
		}
		sort.Strings(phoneDirs)

		tmpl := `<!DOCTYPE html>
<html>
<head>
    <title>Photo Sync Server - Phone Directories</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; background: #f5f5f5; }
        h1 { color: #333; }
        .phone-list { list-style: none; padding: 0; }
        .phone-list li { margin: 10px 0; }
        .phone-list a { 
            display: block; 
            padding: 15px; 
            background: white; 
            text-decoration: none; 
            color: #333; 
            border-radius: 5px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            transition: transform 0.2s;
        }
        .phone-list a:hover { transform: translateX(5px); background: #e3f2fd; }
    </style>
</head>
<body>
    <h1>Photo Sync Server - Phone Directories</h1>
    {{if .PhoneDirs}}
    <ul class="phone-list">
        {{range .PhoneDirs}}
        <li><a href="/phone/{{.}}">📱 {{.}}</a></li>
        {{end}}
    </ul>
    {{else}}
    <p>No phone directories found.</p>
    {{end}}
</body>
</html>`

		t := template.Must(template.New("home").Parse(tmpl))
		data := struct {
			PhoneDirs []string
		}{PhoneDirs: phoneDirs}

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
		const itemsPerPage = 33
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
        body { font-family: Arial, sans-serif; margin: 20px; background: #f5f5f5; }
        h1 { color: #333; }
        .back-link { 
            display: inline-block; 
            margin-bottom: 20px; 
            padding: 10px 20px; 
            background: #2196F3; 
            color: white; 
            text-decoration: none; 
            border-radius: 5px;
        }
        .back-link:hover { background: #1976D2; }
        .info-bar {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 20px;
            flex-wrap: wrap;
            gap: 10px;
        }
        .count { color: #666; margin: 0; }
        .pagination {
            display: flex;
            gap: 5px;
            align-items: center;
        }
        .pagination a, .pagination span {
            padding: 8px 12px;
            border-radius: 3px;
            text-decoration: none;
            background: white;
            color: #333;
            border: 1px solid #ddd;
        }
        .pagination a:hover { background: #e3f2fd; }
        .pagination .current {
            background: #2196F3;
            color: white;
            border-color: #2196F3;
        }
        .pagination .disabled {
            color: #ccc;
            cursor: not-allowed;
        }
        .gallery { 
            display: grid; 
            grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); 
            gap: 15px; 
            padding: 10px;
        }
        .gallery-item { 
            background: white; 
            padding: 10px; 
            border-radius: 5px; 
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            text-align: center;
        }
        .gallery-item img { 
            width: 180px;
            height: 180px;
            object-fit: cover;
            border-radius: 3px;
            cursor: pointer;
        }
        .gallery-item img:hover { opacity: 0.8; }
        .filename { 
            margin-top: 8px; 
            font-size: 12px; 
            color: #666; 
            word-break: break-all;
        }
        .selection-bar {
            position: fixed;
            bottom: 20px;
            right: 20px;
            background: white;
            padding: 15px 20px;
            border-radius: 10px;
            box-shadow: 0 4px 12px rgba(0,0,0,0.2);
            display: none;
            z-index: 1000;
        }
        .selection-bar.active { display: block; }
        .selection-bar button {
            padding: 10px 20px;
            margin: 0 5px;
            border: none;
            border-radius: 5px;
            cursor: pointer;
            font-size: 14px;
        }
        .create-video-btn {
            background: #4CAF50;
            color: white;
        }
        .create-video-btn:hover { background: #45a049; }
        .clear-selection-btn {
            background: #f44336;
            color: white;
        }
        .clear-selection-btn:hover { background: #da190b; }
        .gallery-item.selected {
            border: 3px solid #4CAF50;
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
            background: rgba(0,0,0,0.7);
        }
        #videoModal .modal-content {
            background: white;
            margin: 10% auto;
            padding: 30px;
            width: 500px;
            border-radius: 10px;
        }
        #videoModal h2 { margin-top: 0; }
        #videoModal input, #videoModal select {
            width: 100%;
            padding: 10px;
            margin: 10px 0;
            border: 1px solid #ddd;
            border-radius: 5px;
            box-sizing: border-box;
        }
        #videoModal button {
            padding: 10px 20px;
            margin: 10px 5px 0 0;
            border: none;
            border-radius: 5px;
            cursor: pointer;
        }
        .modal-create { background: #4CAF50; color: white; }
        .modal-create:hover { background: #45a049; }
        .modal-cancel { background: #ccc; color: #333; }
        .modal-cancel:hover { background: #bbb; }
        #videoStatus {
            margin-top: 15px;
            padding: 10px;
            border-radius: 5px;
            display: none;
        }
        #videoStatus.success { background: #d4edda; color: #155724; }
        #videoStatus.error { background: #f8d7da; color: #721c24; }
        #videoStatus.info { background: #d1ecf1; color: #0c5460; }
        
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
        
        /* Video badge in gallery */
        .video-badge {
            position: absolute;
            top: 15px;
            right: 15px;
            background: rgba(0,0,0,0.7);
            color: white;
            padding: 5px 10px;
            border-radius: 3px;
            font-size: 12px;
            z-index: 5;
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
    <div class="info-bar">
        <p class="count">Total: {{.TotalItems}} | Page {{.CurrentPage}} of {{.TotalPages}}</p>
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
                quality: videoQuality
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
		}{
			PhoneName:   phoneName,
			Thumbs:      pagedThumbs,
			TotalItems:  totalItems,
			TotalPages:  totalPages,
			CurrentPage: page,
			PrevPage:    page - 1,
			NextPage:    page + 1,
			PageNumbers: pageNumbers,
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

				// If it's a HEIC file, convert to JPEG on-the-fly for browser compatibility
				if strings.ToLower(ext) == ".heic" {
					log.Printf("Converting HEIC to JPEG for browser: %s", orig)

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
		if err := createVideoFromPhotos(phoneDir, req.Photos, videoName, req.FrameDuration, req.Quality); err != nil {
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
