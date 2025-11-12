package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
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
	version    = "1.0.0"
	tcpPort    = ":9922"
	udpPort    = ":7799"
	bufferSize = 1024
)

// protocol format : type(1 byte) + length(4 bytes big-endian) + payload (JSON or raw string)
// Protocol message types
const (
	msgTypeImageData            byte = 1  // image file payload (JSON with id/data/media)
	msgTypeVideoData            byte = 2  // video file payload (JSON with id/data/media)
	msgTypeSyncComplete         byte = 3  // client indicates sync complete
	msgTypeSetPhoneName         byte = 4  // payload is phone/subdirectory name (raw string)
	msgTypeGetMediaCount        byte = 5  // get total media count request
	msgTypeMediaCountRsp        byte = 6  // response with total media count
	msgTypeMediaThumbList       byte = 7  // request for media thumbnail list (page index and page size in data)
	msgTypeMediaThumbData       byte = 8  // response with media thumbnail data
	msgTypeMediaDelList         byte = 9  // request for media deletion list
	msgTypeMediaDelAck          byte = 10 // acknowledgment for media deletion request
	msgTypeMediaDownloadList    byte = 11 // request for media download
	msgTypeMediaDownloadAck     byte = 12 // acknowledgment for media download request
	msgTypeChunkedVideoStart    byte = 13 // chunked video start - initiates chunked video transfer
	msgTypeChunkedVideoData     byte = 14 // chunked video data - one chunk of video data
	msgTypeChunkedVideoComplete byte = 15 // chunked video complete - all chunks sent

	// Server ACK type (matches client type for simplicity)
	msgTypeAck byte = msgTypeSyncComplete
)

// ChunkedVideoInfo tracks ongoing chunked video transfers
type ChunkedVideoInfo struct {
	ID             string
	TotalSize      int64
	ChunkSize      int
	TotalChunks    int
	ReceivedChunks int
	TempFilePath   string   // temporary file to write chunks
	TempFile       *os.File // file handle
	RecvDir        string
}

// Global state for thumbnail generation control
var (
	thumbnailGenerationMutex sync.Mutex
	thumbnailCancelFunc      context.CancelFunc
	thumbnailCancelMutex     sync.Mutex
)

type Config struct {
	ServerName string `json:"server_name"`
	ReceiveDir string `json:"receive_dir"`
	HttpPort   string `json:"http_port"`
}

func loadConfig(configPath string) (*Config, error) {
	file, err := os.ReadFile(configPath)
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
	case msgTypeImageData:
		return "IMAGE_DATA"
	case msgTypeVideoData:
		return "VIDEO_DATA"
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
	case msgTypeChunkedVideoStart:
		return "CHUNKED_VIDEO_START"
	case msgTypeChunkedVideoData:
		return "CHUNKED_VIDEO_DATA"
	case msgTypeChunkedVideoComplete:
		return "CHUNKED_VIDEO_COMPLETE"
	default:
		return "UNKNOWN"
	}
}

func handleTCPConnection(conn net.Conn, config *Config) {
	// Determine base receive directory from config (fallback to "received")
	baseRecvDir := "received"
	if config != nil && config.ReceiveDir != "" {
		baseRecvDir = config.ReceiveDir
	}

	// Current receive directory (may be modified by msgTypeSetPhoneName)
	recvDir := baseRecvDir

	// Track chunked video transfers for this connection
	chunkedVideos := make(map[string]*ChunkedVideoInfo)

	defer func() {
		log.Printf("Closing connection from %s\n", conn.RemoteAddr().String())

		// Clean up any incomplete chunked video transfers
		for id, info := range chunkedVideos {
			if info.TempFile != nil {
				info.TempFile.Close()
			}
			if info.TempFilePath != "" {
				os.Remove(info.TempFilePath)
				log.Printf("Cleaned up incomplete chunked video temp file for %s", id)
			}
		}

		conn.Close()

		// Trigger thumbnail generation when connection closes
		// Only generate if recvDir has been set (i.e., phone name was received)
		if recvDir != baseRecvDir {
			log.Printf("Connection closed, triggering thumbnail generation for %s\n", recvDir)
			go func(dir string) {
				ctx, cancel := context.WithCancel(context.Background())

				// Store cancel function so it can be called when new sync starts
				thumbnailCancelMutex.Lock()
				thumbnailCancelFunc = cancel
				thumbnailCancelMutex.Unlock()

				if err := generateThumbnails(ctx, dir); err != nil {
					if err == context.Canceled {
						log.Printf("Thumbnail generation cancelled for %s\n", dir)
					} else {
						log.Printf("Thumbnail generation error: %v\n", err)
					}
				} else {
					log.Printf("Thumbnail generation completed for %s\n", dir)
				}

				// Clear cancel function after completion
				thumbnailCancelMutex.Lock()
				thumbnailCancelFunc = nil
				thumbnailCancelMutex.Unlock()
			}(recvDir)
		}
	}()

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

		if msgType != msgTypeImageData && msgType != msgTypeVideoData && msgType != msgTypeSyncComplete && msgType != msgTypeSetPhoneName && msgType != msgTypeGetMediaCount && msgType != msgTypeMediaThumbList && msgType != msgTypeChunkedVideoStart && msgType != msgTypeChunkedVideoData && msgType != msgTypeChunkedVideoComplete {
			log.Printf("Unknown message type %d, closing connection\n", msgType)
			return
		}

		if msgType == msgTypeSyncComplete {
			log.Printf("Received sync complete message type, generating thumbnails under %s\n", recvDir)
			go func() {
				ctx := context.Background()
				if err := generateThumbnails(ctx, recvDir); err != nil {
					log.Printf("Thumbnail generation error: %v\n", err)
				}
			}()
			return
		} // Handle media count request immediately; request payload is ignored if present
		if msgType == msgTypeGetMediaCount {

			count, err := countPhotosInDir(recvDir)
			if err != nil {
				log.Printf("Error counting photos in %s: %v\n", recvDir, err)
				count = 0
			}
			log.Printf("GET Thumbnails count %d \n", count)

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

		// Handle media thumbnail list request: respond with JSON of thumbnails in thumbnails folder, with pagination
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

				// Log full JSON body for MEDIA_THUMB_LIST
				log.Printf("MEDIA_THUMB_LIST payload (JSON): %s", string(tmp))

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

		// Handle chunked video start
		if msgType == msgTypeChunkedVideoStart {
			if length == 0 {
				log.Printf("Received zero-length chunked video start payload, skipping")
				continue
			}

			tmp := make([]byte, length)
			if _, err := io.ReadFull(conn, tmp); err != nil {
				log.Printf("Error reading chunked video start payload: %v\n", err)
				return
			}

			var req struct {
				ID          string `json:"id"`
				Media       string `json:"media"`
				TotalSize   int64  `json:"totalSize"`
				ChunkSize   int    `json:"chunkSize"`
				TotalChunks int    `json:"totalChunks"`
			}
			if err := json.Unmarshal(tmp, &req); err != nil {
				log.Printf("Invalid chunked video start JSON: %v\n", err)
				continue
			}

			log.Printf("Chunked video start: id=%s, totalSize=%d, chunkSize=%d, totalChunks=%d",
				req.ID, req.TotalSize, req.ChunkSize, req.TotalChunks)

			// Create temporary file to write chunks
			tmpFile, err := os.CreateTemp(recvDir, fmt.Sprintf(".chunked_%s_*.tmp",
				strings.ReplaceAll(req.ID, string(filepath.Separator), "_")))
			if err != nil {
				log.Printf("Error creating temp file for chunked video: %v\n", err)
				continue
			}
			tmpPath := tmpFile.Name()
			log.Printf("Created temp file for chunked video: %s", tmpPath)

			// Initialize chunked video tracking
			chunkedVideos[req.ID] = &ChunkedVideoInfo{
				ID:             req.ID,
				TotalSize:      req.TotalSize,
				ChunkSize:      req.ChunkSize,
				TotalChunks:    req.TotalChunks,
				ReceivedChunks: 0,
				TempFilePath:   tmpPath,
				TempFile:       tmpFile,
				RecvDir:        recvDir,
			}

			// Send ACK: OK:START
			ack := []byte("OK:START")
			ackHeader := make([]byte, 5)
			ackHeader[0] = msgTypeAck
			binary.BigEndian.PutUint32(ackHeader[1:5], uint32(len(ack)))
			if _, err := conn.Write(append(ackHeader, ack...)); err != nil {
				log.Printf("Error writing chunked video start ACK: %v\n", err)
			}
			continue
		} // Handle chunked video data
		if msgType == msgTypeChunkedVideoData {
			if length == 0 {
				log.Printf("Received zero-length chunked video data payload, skipping")
				continue
			}

			tmp := make([]byte, length)
			if _, err := io.ReadFull(conn, tmp); err != nil {
				log.Printf("Error reading chunked video data payload: %v\n", err)
				return
			}

			var req struct {
				ID         string `json:"id"`
				ChunkIndex int    `json:"chunkIndex"`
				Data       string `json:"data"`
			}
			if err := json.Unmarshal(tmp, &req); err != nil {
				log.Printf("Invalid chunked video data JSON: %v\n", err)
				continue
			}

			// Decode chunk data
			chunkBytes, err := base64.StdEncoding.DecodeString(req.Data)
			if err != nil {
				log.Printf("Error decoding chunk data for id=%s, chunk=%d: %v\n", req.ID, req.ChunkIndex, err)
				continue
			}

			log.Printf("Received chunk %d for video %s, size=%d bytes", req.ChunkIndex, req.ID, len(chunkBytes))

			// Write chunk to temporary file
			if info, exists := chunkedVideos[req.ID]; exists {
				// Write chunk data to temp file
				if _, err := info.TempFile.Write(chunkBytes); err != nil {
					log.Printf("Error writing chunk to temp file: %v\n", err)
					// Clean up
					info.TempFile.Close()
					os.Remove(info.TempFilePath)
					delete(chunkedVideos, req.ID)
					continue
				}

				info.ReceivedChunks++
				log.Printf("Written chunk %d/%d for video %s to temp file", info.ReceivedChunks, info.TotalChunks, req.ID)
			} else {
				log.Printf("Warning: Received chunk for unknown video ID: %s\n", req.ID)
			}

			// Send ACK: OK:CHUNK:index
			ack := []byte(fmt.Sprintf("OK:CHUNK:%d", req.ChunkIndex))
			ackHeader := make([]byte, 5)
			ackHeader[0] = msgTypeAck
			binary.BigEndian.PutUint32(ackHeader[1:5], uint32(len(ack)))
			if _, err := conn.Write(append(ackHeader, ack...)); err != nil {
				log.Printf("Error writing chunked video data ACK: %v\n", err)
			}
			continue
		}

		// Handle chunked video complete
		if msgType == msgTypeChunkedVideoComplete {
			if length == 0 {
				log.Printf("Received zero-length chunked video complete payload, skipping")
				continue
			}

			tmp := make([]byte, length)
			if _, err := io.ReadFull(conn, tmp); err != nil {
				log.Printf("Error reading chunked video complete payload: %v\n", err)
				return
			}

			var req struct {
				ID          string `json:"id"`
				TotalChunks int    `json:"totalChunks"`
			}
			if err := json.Unmarshal(tmp, &req); err != nil {
				log.Printf("Invalid chunked video complete JSON: %v\n", err)
				continue
			}

			log.Printf("Chunked video complete: id=%s, totalChunks=%d", req.ID, req.TotalChunks)

			// Finalize the video file
			if info, exists := chunkedVideos[req.ID]; exists {
				// Close temp file
				info.TempFile.Close()

				// Verify received chunks count
				if info.ReceivedChunks != info.TotalChunks {
					log.Printf("Warning: Expected %d chunks but received %d for video %s",
						info.TotalChunks, info.ReceivedChunks, req.ID)
				}

				// Determine final filename
				ext := strings.ToLower(filepath.Ext(req.ID))
				if ext == "" {
					ext = ".mp4" // default to mp4
				}

				var fname string
				if strings.ToLower(filepath.Ext(req.ID)) != "" {
					fname = filepath.Join(info.RecvDir, req.ID)
				} else {
					fname = filepath.Join(info.RecvDir, req.ID+ext)
				}

				// Move temp file to final location
				if err := os.Rename(info.TempFilePath, fname); err != nil {
					log.Printf("Error moving temp file to final location %s: %v\n", fname, err)
					// Try copy and delete as fallback
					if copyErr := copyFile(info.TempFilePath, fname); copyErr != nil {
						log.Printf("Error copying temp file: %v\n", copyErr)
					} else {
						os.Remove(info.TempFilePath)
						// Get file size
						if fileInfo, statErr := os.Stat(fname); statErr == nil {
							log.Printf("Saved chunked video: %s (size=%d bytes, chunks=%d)\n",
								fname, fileInfo.Size(), info.TotalChunks)
						}
					}
				} else {
					// Get file size
					if fileInfo, err := os.Stat(fname); err == nil {
						log.Printf("Saved chunked video: %s (size=%d bytes, chunks=%d)\n",
							fname, fileInfo.Size(), info.TotalChunks)
					}
				}

				// Clean up tracking
				delete(chunkedVideos, req.ID)
			} else {
				log.Printf("Warning: Received complete signal for unknown video ID: %s\n", req.ID)
			}

			// Send ACK: OK:video_id
			ack := []byte("OK:" + req.ID)
			ackHeader := make([]byte, 5)
			ackHeader[0] = msgTypeAck
			binary.BigEndian.PutUint32(ackHeader[1:5], uint32(len(ack)))
			if _, err := conn.Write(append(ackHeader, ack...)); err != nil {
				log.Printf("Error writing chunked video complete ACK: %v\n", err)
			}
			continue
		}

		if length == 0 {
			log.Printf("Received zero-length payload, skipping")
			continue
		}

		if length > 500*1024*1024 { // limit 500MB for safety (to handle large videos)
			log.Printf("Payload too large (%d bytes), closing connection\n", length)
			return
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			log.Printf("Error reading payload: %v\n", err)
			return
		}

		if msgType == msgTypeSetPhoneName {
			// Cancel any running thumbnail generation when new sync starts
			thumbnailCancelMutex.Lock()
			if thumbnailCancelFunc != nil {
				log.Printf("Cancelling ongoing thumbnail generation (new sync starting)")
				thumbnailCancelFunc()
			}
			thumbnailCancelMutex.Unlock()

			//client phone name is in this request,
			phoneName := string(payload)
			log.Printf("SET_PHONE_NAME payload (full string): %s", phoneName)
			//create a sub directory under receive dir
			recvDir = filepath.Join(baseRecvDir, phoneName)
			if err := os.MkdirAll(recvDir, 0o755); err != nil {
				log.Printf("Error creating receive dir: %v\n", err)
				return
			}
			continue
		} // Parse JSON
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

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
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
} // generateThumbnails scans the phone directory and writes thumbnails into a subdirectory named "thumbnails".
// For photos (jpg/jpeg/png): thumbnails keep the original extension and are named with prefix "tbn-".
// For videos (mp4/mov/m4v/avi/mkv): thumbnails are JPEG files named "tbn-<original-basename>.jpg".
func generateThumbnails(ctx context.Context, parentDir string) error {
	// Acquire lock to ensure only one thumbnail generation at a time
	thumbnailGenerationMutex.Lock()
	defer thumbnailGenerationMutex.Unlock()

	log.Printf("Starting thumbnail generation for %s (acquired lock)", parentDir)

	thumbDir := filepath.Join(parentDir, "thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return fmt.Errorf("creating thumbnails dir: %w", err)
	}

	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return fmt.Errorf("read parent dir: %w", err)
	}

	for _, e := range entries {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			log.Printf("Thumbnail generation cancelled for %s", parentDir)
			return ctx.Err()
		default:
		}

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
			// For HEIC files, thumbnail will be saved as .jpg
			thumbName := name
			if ext == ".heic" {
				// Replace .heic extension with .jpg for thumbnail
				base := strings.TrimSuffix(name, ext)
				thumbName = base + ".jpg"
			}
			thumbPath := filepath.Join(thumbDir, "tbn-"+thumbName)
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
			// HEIC files are converted to JPEG, so encode as JPEG
			// PNG files keep PNG format, all others (including HEIC) use JPEG
			if ext == ".png" && !isHEIC {
				if err := png.Encode(out, thumbImg); err != nil {
					log.Printf("encode png failed %s: %v", thumbPath, err)
				}
			} else {
				// jpg/jpeg/heic and others -> jpeg
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
	thumbDir := filepath.Join(dir, "thumbnails")
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte(`{"photos":[]}`), nil
		}
		return nil, fmt.Errorf("read thumbnails dir: %w", err)
	}

	// Filter to image files only and sort stably by name
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".heic" {
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

// countPhotosInDir returns the number of thumbnail files in the thumbnails directory.
// This counts jpg, jpeg, png, and heic thumbnails.
func countPhotosInDir(dir string) (int, error) {
	thumbDir := filepath.Join(dir, "thumbnails")
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".heic" {
			count++
		}
	}
	return count, nil
}

func main() {
	// Parse command-line flags
	showVersion := flag.Bool("v", false, "show version and exit")
	configPath := flag.String("f", "config.json", "path to config file")
	flag.Parse()

	// Show version and exit if requested
	if *showVersion {
		fmt.Printf("Photo Sync Server version %s\n", version)
		os.Exit(0)
	}

	// Load configuration
	config, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("Error loading config from %s: %v\n", *configPath, err)
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
