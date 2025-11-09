package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

const (
	tcpPort    = ":9922"
	udpPort    = ":7799"
	bufferSize = 1024
)

// protocol format : type(1 byte) + length(4 bytes big-endian) + payload (JSON or raw string)
// Protocol message types
const (
	msgTypeData              byte = 1  // file payload (JSON with id/data/media)
	msgTypeDataAlt           byte = 2  // alternate payload type (handled same as type 1)
	msgTypeSyncComplete      byte = 3  // client indicates sync complete
	msgTypeSetPhoneName      byte = 4  // payload is phone/subdirectory name (raw string)
	msgTypeGetMediaCount     byte = 5  // get total media count request
	msgTypeMediaCountRsp     byte = 6  // response with total media count
	msgTypeMediaThumbList    byte = 7  // request for media thumbnail list (page index and page size in data)
	msgTypeMediaThumbData    byte = 8  // response with media thumbnail data
	msgTypeMediaDelList      byte = 9  // request for media deletion list
	msgTypeMediaDelAck       byte = 10 // acknowledgment for media deletion request
	msgTypeMediaDownloadList byte = 11 // request for media download
	msgTypeMediaDownloadAck  byte = 12 // acknowledgment for media download request

	// Server ACK type (matches client type for simplicity)
	msgTypeAck byte = msgTypeSyncComplete
)

type Config struct {
	ServerName string `json:"server_name"`
	ReceiveDir string `json:"receive_dir"`
	HttpPort   string `json:"http_port"`
}

func loadConfig() (*Config, error) {
	file, err := os.ReadFile("config.json")
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	return &config, nil
}

type NetworkInfo struct {
	IP        net.IP
	Broadcast net.IP
}

func getDefaultInterfaceInfo() (*NetworkInfo, error) {
	// First try to get a connection to a known public IP to determine default route
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, fmt.Errorf("failed to determine default interface: %v", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	defaultIP := localAddr.IP

	// Now find the interface that has this IP
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("error getting network interfaces: %v", err)
	}

	for _, iface := range interfaces {
		// Skip loopback and non-up interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					if ip4.Equal(defaultIP) {
						// Found the default interface
						broadcast := net.IP(make([]byte, 4))
						for i := range ip4 {
							broadcast[i] = ip4[i] | ^ipnet.Mask[i]
						}
						log.Printf("Found default interface %s with IP %s\n", iface.Name, ip4.String())
						return &NetworkInfo{
							IP:        ip4,
							Broadcast: broadcast,
						}, nil
					}
				}
			}
		}
	}

	return nil, fmt.Errorf("no suitable network interface found")
}

// getMsgTypeName returns a readable name for the message type
func getMsgTypeName(msgType byte) string {
	switch msgType {
	case msgTypeData:
		return "DATA"
	case msgTypeDataAlt:
		return "DATA_ALT"
	case msgTypeSyncComplete:
		return "SYNC_COMPLETE"
	case msgTypeSetPhoneName:
		return "SET_PHONE_NAME"
	case msgTypeGetMediaCount:
		return "GET_MEDIA_COUNT"
	case msgTypeMediaCountRsp:
		return "MEDIA_COUNT_RSP"
	case msgTypeMediaThumbList:
		return "MEDIA_THUMB_LIST"
	case msgTypeMediaThumbData:
		return "MEDIA_THUMB_DATA"
	case msgTypeMediaDelList:
		return "MEDIA_DEL_LIST"
	case msgTypeMediaDelAck:
		return "MEDIA_DEL_ACK"
	case msgTypeMediaDownloadList:
		return "MEDIA_DOWNLOAD_LIST"
	case msgTypeMediaDownloadAck:
		return "MEDIA_DOWNLOAD_ACK"
	default:
		return "UNKNOWN"
	}
}

func handleTCPConnection(conn net.Conn, config *Config) {
	defer func() {
		log.Printf("Closing connection from %s\n", conn.RemoteAddr().String())
		conn.Close()
	}()

	// Determine base receive directory from config (fallback to "received")
	baseRecvDir := "received"
	if config != nil && config.ReceiveDir != "" {
		baseRecvDir = config.ReceiveDir
	}

	// Current receive directory (may be modified by msgTypeSetPhoneName)
	recvDir := baseRecvDir

	// Protocol: 1 byte type, 4 bytes length (big-endian uint32), then payload
	// Payload is JSON. JSON: {"id":"...","data":"<base64>","media":"jpg"}
	for {
		// Read header: 1 + 4 bytes
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			if err != io.EOF {
				log.Printf("Error reading header from TCP connection: %v\n", err)
			}
			return
		}

		msgType := header[0]
		length := binary.BigEndian.Uint32(header[1:5])

		// Get readable message type name
		msgTypeName := getMsgTypeName(msgType)

		// Log request header info
		log.Printf("Request: type=%s(%d), len=%d", msgTypeName, msgType, length)

		if msgType != msgTypeData && msgType != msgTypeDataAlt && msgType != msgTypeSyncComplete && msgType != msgTypeSetPhoneName && msgType != msgTypeGetMediaCount && msgType != msgTypeMediaThumbList {
			log.Printf("Unknown message type %d, closing connection\n", msgType)
			return
		}

		if msgType == msgTypeSyncComplete {
			log.Printf("Received sync complete message type, generating thumbnails under %s\n", recvDir)
			go func() {
				if err := generateThumbnails(recvDir); err != nil {
					log.Printf("Thumbnail generation error: %v\n", err)
				}
			}()
			return
		}

		// Handle media count request immediately; request payload is ignored if present
		if msgType == msgTypeGetMediaCount {
			if length > 0 {
				// Read and discard request payload
				tmp := make([]byte, length)
				if _, err := io.ReadFull(conn, tmp); err != nil {
					log.Printf("Error discarding media count payload: %v\n", err)
					return
				}
				// Log first 10 bytes
				previewLen := 10
				if len(tmp) < previewLen {
					previewLen = len(tmp)
				}
				preview := string(tmp[:previewLen])
				previewBytes := []byte(preview)
				for i := range previewBytes {
					if previewBytes[i] < 32 || previewBytes[i] > 126 {
						previewBytes[i] = '.'
					}
				}
				log.Printf("  Payload preview (first %d bytes): %q", previewLen, string(previewBytes))
			}

			count, err := countPhotosInDir(recvDir)
			if err != nil {
				log.Printf("Error counting photos in %s: %v\n", recvDir, err)
				count = 0
			}

			data := make([]byte, 4)
			binary.BigEndian.PutUint32(data, uint32(count))
			respHeader := make([]byte, 5)
			respHeader[0] = msgTypeMediaCountRsp
			binary.BigEndian.PutUint32(respHeader[1:5], uint32(len(data)))
			if _, err := conn.Write(append(respHeader, data...)); err != nil {
				log.Printf("Error sending media count response: %v\n", err)
			}
			continue
		}

		// Handle media thumbnail list request: respond with JSON of thumbnails in subnails, with pagination
		if msgType == msgTypeMediaThumbList {
			// Defaults
			pageIndex := 0
			pageSize := 100

			if length > 0 {
				// Read request payload and parse pagination
				tmp := make([]byte, length)
				if _, err := io.ReadFull(conn, tmp); err != nil {
					log.Printf("Error reading thumb list payload: %v\n", err)
					return
				}
				// Log first 10 bytes
				previewLen := 10
				if len(tmp) < previewLen {
					previewLen = len(tmp)
				}
				preview := string(tmp[:previewLen])
				previewBytes := []byte(preview)
				for i := range previewBytes {
					if previewBytes[i] < 32 || previewBytes[i] > 126 {
						previewBytes[i] = '.'
					}
				}
				log.Printf("  Payload preview (first %d bytes): %q", previewLen, string(previewBytes))

				var req struct {
					PageIndex int `json:"pageIndex"`
					PageSize  int `json:"pageSize"`
				}
				if err := json.Unmarshal(tmp, &req); err != nil {
					log.Printf("Invalid thumb list JSON, using defaults: %v\n", err)
				} else {
					if req.PageIndex >= 0 {
						pageIndex = req.PageIndex
					}
					if req.PageSize > 0 {
						pageSize = req.PageSize
					}
				}
			}

			payload, err := buildThumbsJSONPayloadPaged(recvDir, pageIndex, pageSize)
			if err != nil {
				log.Printf("Error building thumbnails JSON: %v\n", err)
				// On error, still send an empty list
				payload = []byte(`{"photos":[]}`)
			}

			respHeader := make([]byte, 5)
			respHeader[0] = msgTypeMediaThumbData
			binary.BigEndian.PutUint32(respHeader[1:5], uint32(len(payload)))
			if _, err := conn.Write(append(respHeader, payload...)); err != nil {
				log.Printf("Error sending thumbnail list response: %v\n", err)
			}
			continue
		}
		if length == 0 {
			log.Printf("Received zero-length payload, skipping")
			continue
		}

		if length > 50*1024*1024 { // limit 50MB for safety
			log.Printf("Payload too large (%d bytes), closing connection\n", length)
			return
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			log.Printf("Error reading payload: %v\n", err)
			return
		}

		// Log first 10 bytes of payload as string (for debugging)
		previewLen := 10
		if len(payload) < previewLen {
			previewLen = len(payload)
		}
		preview := string(payload[:previewLen])
		// Replace non-printable characters with '.'
		previewBytes := []byte(preview)
		for i := range previewBytes {
			if previewBytes[i] < 32 || previewBytes[i] > 126 {
				previewBytes[i] = '.'
			}
		}
		log.Printf("  Payload preview (first %d bytes): %q", previewLen, string(previewBytes))

		if msgType == msgTypeSetPhoneName {
			//client phone name is in this request,
			phoneName := string(payload)
			log.Printf("Client phone name: %s\n", phoneName)
			//create a sub directory under receive dir
			recvDir = filepath.Join(baseRecvDir, phoneName)
			if err := os.MkdirAll(recvDir, 0o755); err != nil {
				log.Printf("Error creating receive dir: %v\n", err)
				return
			}
			continue
		}

		// Parse JSON
		var obj struct {
			ID    string `json:"id"`
			Data  string `json:"data"`
			Media string `json:"media"`
		}
		if err := json.Unmarshal(payload, &obj); err != nil {
			log.Printf("Error unmarshaling JSON payload: %v\n", err)
			continue
		}

		if obj.ID == "" || obj.Data == "" || obj.Media == "" {
			log.Printf("Invalid payload fields: id/data/media required\n")
			continue
		}

		// Decode base64 data
		fileBytes, err := base64.StdEncoding.DecodeString(obj.Data)
		if err != nil {
			log.Printf("Error decoding base64 data for id=%s: %v\n", obj.ID, err)
			continue
		}

		// Log decoded file info and first 16 bytes for validation
		log.Printf("Decoded file id=%s, size=%d bytes, base64_len=%d", obj.ID, len(fileBytes), len(obj.Data))
		if len(fileBytes) > 0 {
			previewBytes := 16
			if len(fileBytes) < previewBytes {
				previewBytes = len(fileBytes)
			}
			log.Printf("  First %d bytes: %x", previewBytes, fileBytes[:previewBytes])
		}

		// Save to <recvDir>/<id>.<ext>
		ext := strings.ToLower(obj.Media)
		// sanitize ext to prevent path issues: keep letters/numbers
		if strings.ContainsAny(ext, "/\\") || ext == "" {
			ext = "bin"
		}

		// Check if ID already has the extension to avoid double extensions
		var fname string
		idExt := strings.ToLower(filepath.Ext(obj.ID))
		expectedExt := "." + ext
		if idExt == expectedExt {
			// ID already has the correct extension
			fname = filepath.Join(recvDir, obj.ID)
		} else {
			// Need to add extension
			fname = filepath.Join(recvDir, fmt.Sprintf("%s.%s", obj.ID, ext))
		}

		// Create parent directories if obj.ID contains path separators
		if dir := filepath.Dir(fname); dir != recvDir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("Error creating directory for id=%s: %v\n", obj.ID, err)
				continue
			}
		}

		if err := os.WriteFile(fname, fileBytes, 0o644); err != nil {
			log.Printf("Error saving file for id=%s: %v\n", obj.ID, err)
			continue
		}

		log.Printf("Saved received file: %s (type=%d size=%d bytes)\n", fname, msgType, len(fileBytes))

		// Send a simple ACK back, payload format: OK:<id>
		// Simple ACK format: type 3, length, payload
		ack := []byte("OK:" + obj.ID)
		// Prepend simple framing for ACK (type msgTypeAck with length)
		ackHeader := make([]byte, 5)
		ackHeader[0] = msgTypeAck
		binary.BigEndian.PutUint32(ackHeader[1:5], uint32(len(ack)))
		if _, err := conn.Write(append(ackHeader, ack...)); err != nil {
			log.Printf("Error writing ACK to client: %v\n", err)
		}
	}
}

func startTCPServer(config *Config) error {
	listener, err := net.Listen("tcp", tcpPort)
	if err != nil {
		return fmt.Errorf("failed to start TCP server: %v", err)
	}
	defer listener.Close()

	log.Printf("TCP Server listening on port%s\n", tcpPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting TCP connection: %v\n", err)
			continue
		}

		log.Printf("New TCP connection from %s\n", conn.RemoteAddr().String())
		go handleTCPConnection(conn, config)
	}
}

func startUDPServer(config *Config) error {
	// Get network interface information
	netInfo, err := getDefaultInterfaceInfo()
	if err != nil {
		return fmt.Errorf("failed to get network interface info: %v", err)
	}

	// Set up UDP broadcast address for listening
	addr := &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0), // Listen on all available interfaces
		Port: 7799,
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to start UDP server: %v", err)
	}
	defer conn.Close()

	log.Printf("UDP Server listening on port%s\n", udpPort)
	log.Printf("UDP Server IP: %s, Broadcast: %s\n", netInfo.IP.String(), netInfo.Broadcast.String())

	buffer := make([]byte, bufferSize)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Printf("Error reading from UDP: %v\n", err)
			continue
		}

		data := string(buffer[:n])
		log.Printf("Received UDP data from %s: %s\n", remoteAddr.String(), data)

		// Check if this is a server discovery request
		if strings.TrimSpace(data) == "who is photo server?" {
			response := fmt.Sprintf("photo_server:%s,IP:%s", config.ServerName, netInfo.IP.String())

			// Send response to both the requester and broadcast address
			_, err = conn.WriteToUDP([]byte(response), remoteAddr)
			if err != nil {
				log.Printf("Error sending server info response to requester: %v\n", err)
			}

			// Also send to broadcast address
			broadcastAddr := &net.UDPAddr{
				IP:   netInfo.Broadcast,
				Port: remoteAddr.Port,
			}
			_, err = conn.WriteToUDP([]byte(response), broadcastAddr)
			if err != nil {
				log.Printf("Error sending server info response to broadcast: %v\n", err)
			}
			continue
		}

		// Echo back other messages
		_, err = conn.WriteToUDP(buffer[:n], remoteAddr)
		if err != nil {
			log.Printf("Error sending UDP response: %v\n", err)
		}
	}
}

// convertHEICToImage converts a HEIC file to JPEG using ImageMagick and returns the decoded image
func convertHEICToImage(heicPath string) (image.Image, string, error) {
	// Create a temporary JPEG file
	tmpFile, err := os.CreateTemp("", "heic-convert-*.jpg")
	if err != nil {
		return nil, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	var cmd *exec.Cmd
	var conversionMethod string

	// Try ImageMagick first (best HEIC support)
	if _, err := exec.LookPath("magick"); err == nil {
		cmd = exec.Command("magick", "convert", heicPath, tmpPath)
		conversionMethod = "ImageMagick"
	} else if _, err := exec.LookPath("convert"); err == nil {
		// Try older ImageMagick command
		cmd = exec.Command("convert", heicPath, tmpPath)
		conversionMethod = "ImageMagick (convert)"
	} else {
		return nil, "", fmt.Errorf("ImageMagick not found. Please install ImageMagick for HEIC support")
	}

	log.Printf("Converting HEIC using %s: %s", conversionMethod, heicPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, "", fmt.Errorf("%s conversion failed: %w, output: %s", conversionMethod, err, string(output))
	}

	// Open and decode the converted JPEG
	f, err := os.Open(tmpPath)
	if err != nil {
		return nil, "", fmt.Errorf("open converted image: %w", err)
	}
	defer f.Close()

	img, format, err := image.Decode(f)
	if err != nil {
		return nil, "", fmt.Errorf("decode converted image: %w", err)
	}

	log.Printf("Successfully converted HEIC to %s using %s", format, conversionMethod)
	return img, format, nil
} // generateThumbnails scans the phone directory and writes thumbnails into a subdirectory named "subnails".
// For photos (jpg/jpeg/png): thumbnails keep the original extension and are named with prefix "tbn-".
// For videos (mp4/mov/m4v/avi/mkv): thumbnails are JPEG files named "tbn-<original-basename>.jpg".
func generateThumbnails(parentDir string) error {
	thumbDir := filepath.Join(parentDir, "subnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return fmt.Errorf("creating subnails dir: %w", err)
	}

	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return fmt.Errorf("read parent dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(strings.ToLower(name), "tbn-") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		srcPath := filepath.Join(parentDir, name)

		// Handle images
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".heic" {
			thumbPath := filepath.Join(thumbDir, "tbn-"+name)
			if _, err := os.Stat(thumbPath); err == nil {
				// already exists
				continue
			}

			// Check if file is actually HEIC (even if extension says .jpg)
			isHEIC := false
			if f, err := os.Open(srcPath); err == nil {
				header := make([]byte, 12)
				n, _ := io.ReadFull(f, header)
				f.Close()
				// HEIC files start with: ftyp (at offset 4)
				if n >= 12 && string(header[4:8]) == "ftyp" {
					heicType := string(header[8:12])
					log.Printf("File %s has ftyp signature, type: %q (hex: %x)", name, heicType, header)
					if heicType == "heic" || heicType == "heix" || heicType == "mif1" {
						isHEIC = true
					}
				} else if n > 0 {
					log.Printf("File %s header (first %d bytes): %x", name, n, header[:n])
				}
			}

			var img image.Image
			var format string
			var err error

			if isHEIC {
				// Convert HEIC to JPEG using ffmpeg, then decode
				img, format, err = convertHEICToImage(srcPath)
				if err != nil {
					log.Printf("failed to convert HEIC %s: %v", srcPath, err)
					continue
				}
			} else {
				// Standard image decoding
				f, err := os.Open(srcPath)
				if err != nil {
					log.Printf("open source image failed %s: %v", srcPath, err)
					continue
				}

				img, format, err = image.Decode(f)
				_ = f.Close()
				if err != nil {
					// Check file size and first few bytes for debugging
					info, _ := os.Stat(srcPath)
					firstBytes := make([]byte, 16)
					if tmpF, tmpErr := os.Open(srcPath); tmpErr == nil {
						io.ReadFull(tmpF, firstBytes)
						tmpF.Close()
						log.Printf("decode image failed %s (size: %d, format detected: %s, first bytes: %x): %v",
							srcPath, info.Size(), format, firstBytes, err)
					} else {
						log.Printf("decode image failed %s: %v", srcPath, err)
					}
					continue
				}
			}

			// calculate thumbnail size (max width 320px, keep aspect)
			b := img.Bounds()
			w := b.Dx()
			h := b.Dy()
			maxW := 320
			newW := w
			newH := h
			if w > maxW {
				ratio := float64(maxW) / float64(w)
				newW = maxW
				newH = int(float64(h) * ratio)
			}
			if newW <= 0 {
				newW = 1
			}
			if newH <= 0 {
				newH = 1
			}

			thumbImg := image.NewRGBA(image.Rect(0, 0, newW, newH))
			draw.CatmullRom.Scale(thumbImg, thumbImg.Bounds(), img, img.Bounds(), draw.Over, nil)

			out, err := os.Create(thumbPath)
			if err != nil {
				log.Printf("create thumbnail failed %s: %v", thumbPath, err)
				continue
			}
			switch ext {
			case ".png":
				if err := png.Encode(out, thumbImg); err != nil {
					log.Printf("encode png failed %s: %v", thumbPath, err)
				}
			default: // jpg/jpeg and others -> jpeg
				if err := jpeg.Encode(out, thumbImg, &jpeg.Options{Quality: 80}); err != nil {
					log.Printf("encode jpeg failed %s: %v", thumbPath, err)
				}
			}
			_ = out.Close()
			log.Printf("thumbnail written: %s", thumbPath)
			continue
		}

		// Handle videos (use ffmpeg if available)
		if ext == ".mp4" || ext == ".mov" || ext == ".m4v" || ext == ".avi" || ext == ".mkv" {
			// Check if this video was created by the video creation feature
			base := strings.TrimSuffix(name, ext)
			markerPath := filepath.Join(parentDir, "."+base+".created")
			if _, err := os.Stat(markerPath); err == nil {
				// This video was created from photos, skip thumbnail generation
				log.Printf("Skipping thumbnail for created video: %s", name)
				continue
			}

			thumbPath := filepath.Join(thumbDir, "tbn-"+base+".jpg")
			if _, err := os.Stat(thumbPath); err == nil {
				// already exists
				continue
			}
			if err := generateVideoThumbnail(srcPath, thumbPath); err != nil {
				log.Printf("video thumbnail failed %s -> %s: %v", srcPath, thumbPath, err)
			} else {
				log.Printf("thumbnail written: %s", thumbPath)
			}
			continue
		}
		// Other file types: skip
	}
	return nil
}

// generateVideoThumbnail uses ffmpeg CLI to extract a frame and scale it to width 320 (preserving aspect).
func generateVideoThumbnail(srcPath, dstPath string) error {
	// Ensure ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	// Use a context with timeout to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// ffmpeg -y -ss 00:00:01 -i input -frames:v 1 -vf "scale=320:-1" output.jpg
	cmd := exec.CommandContext(
		ctx, "ffmpeg",
		"-y",
		"-ss", "00:00:01",
		"-i", srcPath,
		"-frames:v", "1",
		"-vf", "scale=320:-1",
		dstPath,
	)
	// Reduce noise: redirect stdout/stderr to files or discard
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// buildThumbsJSONPayloadPaged is like buildThumbsJSONPayload but returns only a page
// of thumbnails based on pageIndex (0-based) and pageSize. Stable order by filename.
func buildThumbsJSONPayloadPaged(dir string, pageIndex, pageSize int) ([]byte, error) {
	thumbDir := filepath.Join(dir, "subnails")
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte(`{"photos":[]}`), nil
		}
		return nil, fmt.Errorf("read subnails dir: %w", err)
	}

	// Filter to image files only and sort stably by name
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
			names = append(names, e.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return names[i] < names[j] })

	// Sanitize pagination
	if pageIndex < 0 {
		pageIndex = 0
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	start := pageIndex * pageSize
	if start >= len(names) {
		return []byte(`{"photos":[]}`), nil
	}
	end := start + pageSize
	if end > len(names) {
		end = len(names)
	}
	page := names[start:end]

	type photoItem struct {
		ID    string `json:"id"`
		Data  string `json:"data"`
		Media string `json:"media"`
	}
	type payload struct {
		Photos []photoItem `json:"photos"`
	}
	out := payload{Photos: make([]photoItem, 0, len(page))}

	for _, name := range page {
		ext := strings.ToLower(filepath.Ext(name))
		b, err := os.ReadFile(filepath.Join(thumbDir, name))
		if err != nil {
			log.Printf("read thumb failed %s: %v", name, err)
			continue
		}
		base := strings.TrimSuffix(name, ext)
		if strings.HasPrefix(strings.ToLower(base), "tbn-") {
			base = base[4:]
		}

		// Determine media type by checking if original file is a video
		media := strings.TrimPrefix(ext, ".")
		if media == "jpeg" {
			media = "jpg"
		}

		// Check if the original file (in parent dir) is a video
		// Look for common video extensions
		videoExts := []string{".mp4", ".mov", ".m4v", ".avi", ".mkv"}
		isVideo := false
		for _, vext := range videoExts {
			origPath := filepath.Join(dir, base+vext)
			if _, err := os.Stat(origPath); err == nil {
				isVideo = true
				break
			}
		}

		if isVideo {
			media = "video"
		}

		out.Photos = append(out.Photos, photoItem{
			ID:    base,
			Data:  base64.StdEncoding.EncodeToString(b),
			Media: media,
		})
	}
	return json.Marshal(out)
}

// countPhotosInDir returns the number of photo files directly under dir (non-recursive),
// excluding the thumbnails directory ("subnails"). Photo extensions considered: jpg, jpeg, png.
func countPhotosInDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			// skip any subdirectory, especially the thumbnails directory
			if strings.EqualFold(e.Name(), "subnails") {
				continue
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".mp4" || ext == ".mov" || ext == ".m4v" || ext == ".avi" || ext == ".mkv" {
			count++
		}
	}
	return count, nil
}

// getSortedFileList returns a stable-sorted list of filenames (not directories) in the given directory.
// Files are sorted lexicographically by name. Uses stable sort to preserve original order for equal names.
func getSortedFileList(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}

	// Stable sort by filename
	sort.SliceStable(files, func(i, j int) bool {
		return files[i] < files[j]
	})

	return files, nil
}

func main() {
	// Load configuration
	config, err := loadConfig()
	if err != nil {
		log.Printf("Error loading config: %v\n", err)
		config = &Config{ServerName: "unknown"} // Use default name if config fails
	}

	log.Printf("Server Name: %s\n", config.ServerName)

	var wg sync.WaitGroup
	wg.Add(3)

	// Start TCP server
	go func() {
		defer wg.Done()
		if err := startTCPServer(config); err != nil {
			log.Printf("TCP Server error: %v\n", err)
		}
	}()

	// Start UDP server
	go func() {
		defer wg.Done()
		if err := startUDPServer(config); err != nil {
			log.Printf("UDP Server error: %v\n", err)
		}
	}()

	// Start HTTP server
	go func() {
		defer wg.Done()
		if err := startHTTPServer(config); err != nil {
			log.Printf("HTTP Server error: %v\n", err)
		}
	}()

	log.Println("Servers starting...")
	wg.Wait()
}
