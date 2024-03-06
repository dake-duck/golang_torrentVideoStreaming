package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/gorilla/websocket"
)

var (
	upgrader      = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	torrentClient *torrent.Client
	videos        map[string]*Video
)

// Video struct represents a video being processed or ready to stream.
type Video struct {
	ID               string
	Name             string
	Status           string
	DownloadProgress float64 // Download progress (percentage)
	ProcessingStatus string  // Processing status ("In Progress" or "Done")
	MagnetLink       string
}

func init() {
	initTorrentClient()

	videos = make(map[string]*Video)

	loadedVideos, err := loadVideosFromFile()
	if err != nil {
		log.Printf("Error loading videos from file: %v", err)
	} else {
		videos = loadedVideos
	}
}

const videosFilePath = "videos.json"

func saveVideosToFile(videos map[string]*Video) error {
	videoData, err := json.MarshalIndent(videos, "", "    ")
	if err != nil {
		return err
	}

	return os.WriteFile(videosFilePath, videoData, 0644)
}

func loadVideosFromFile() (map[string]*Video, error) {
	fileData, err := os.ReadFile(videosFilePath)
	if err != nil {
		return nil, err
	}

	var loadedVideos map[string]*Video
	if err := json.Unmarshal(fileData, &loadedVideos); err != nil {
		return nil, err
	}

	return loadedVideos, nil
}

func resumeDownloadAndProcess(video *Video) {
	// Check if the video is in a state that can be resumed
	if video.Status != "Downloading" && video.ProcessingStatus != "In Progress" {
		return
	}

	t, err := torrentClient.AddMagnet(video.MagnetLink)
	if err != nil {
		return
	}

	go downloadAndProcess(t, video)
}

func initTorrentClient() {
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = "tmp/torrent_data" // Change this to your preferred directory
	var err error
	torrentClient, err = torrent.NewClient(clientConfig)
	if err != nil {
		log.Fatal(err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("stream_id") {
		stream_id := r.URL.Query().Get("stream_id")
		files := []string{
			"./ui/pages/stream.page.tmpl",
			"./ui/layouts/base.layout.tmpl",
		}
		ts, err := template.ParseFiles(files...)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Internal Server Error", 500)
			return
		}

		err = ts.Execute(w, stream_id)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Internal Server Error", 500)
			return
		}
	} else {
		files := []string{
			"./ui/pages/index.page.tmpl",
			"./ui/layouts/base.layout.tmpl",
		}
		ts, err := template.ParseFiles(files...)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Internal Server Error", 500)
			return
		}

		err = ts.Execute(w, videos)
		if err != nil {
			log.Println(err.Error())
			http.Error(w, "Internal Server Error", 500)
			return
		}
	}

}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	for {
		time.Sleep(time.Second * 2)

		// Collect video information
		videoData, err := json.Marshal(videos)

		if err != nil {
			log.Printf("Error encoding video data: %v", err)
			continue
		}

		// Send video information to WebSocket
		err = conn.WriteMessage(websocket.TextMessage, videoData)
		if err != nil {
			break
		}
	}
}

func submitHandler(w http.ResponseWriter, r *http.Request) {
	magnetLink := r.FormValue("magnetLink")
	if magnetLink == "" {
		http.Error(w, "Magnet link is required", http.StatusBadRequest)
		return
	}

	log.Println(magnetLink)
	t, err := torrentClient.AddMagnet(magnetLink)
	<-t.GotInfo()
	log.Println(t.Name())

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	videoID := fmt.Sprintf("%x", t.InfoHash())
	videoName := filepath.Base(t.Name())
	video := Video{ID: videoID, Name: videoName, Status: "Downloading", DownloadProgress: 0, MagnetLink: magnetLink}

	videos[videoID] = &video

	go downloadAndProcess(t, &video)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func downloadAndProcess(t *torrent.Torrent, video *Video) {
	// Define HLS output directory
	hlsDir := filepath.Join("tmp/hls", video.ID)
	if err := os.MkdirAll(hlsDir, os.ModePerm); err != nil {
		log.Printf("Error creating HLS directory: %v", err)
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	go func() {
		for range ticker.C {
			downloadProgress := float64(t.BytesCompleted()) / float64(t.Length()) * 100
			video.DownloadProgress = downloadProgress
		}
	}()

	t.DownloadAll()

	select {
	case <-t.Complete.On():
		// Wait for the download to complete
		ticker.Stop()

		video.Status = "Processing"
		video.DownloadProgress = 100 // Set download progress to 100% after download is complete
		video.ProcessingStatus = "In Progress"

		var inputFile string
		var outputFile string

		if len(t.Files()) == 1 {
			// Single file torrent
			inputFile = filepath.Join("tmp/torrent_data", t.Name())
			outputFile = filepath.Join(hlsDir, "output.m3u8")
		} else if len(t.Files()) > 1 {
			// Directory with multiple files
			videoDir := filepath.Join("tmp/torrent_data", t.Name())
			inputFile, outputFile = processVideoDirectory(videoDir, hlsDir)
		} else {
			log.Printf("Torrent does not contain any files.")
			video.Status = "Error"
			return
		}

		cmd := exec.Command("ffmpeg",
			"-i", inputFile,
			"-c:v", "libx264",
			"-profile:v", "baseline",
			"-hls_time", "20",
			"-hls_playlist_type", "vod",
			"-hls_segment_type", "mpegts",
			"-hls_flags", "independent_segments",
			"-hls_list_size", "0",
			"-hls_segment_filename", filepath.Join(hlsDir, "segment%d.ts"),
			outputFile,
		)
		err := cmd.Run()
		if err != nil {
			log.Printf("Error processing video: %v", err)
			video.Status = "Error"
			return
		}

		video.Status = "Ready"
		video.ProcessingStatus = "Done"
		log.Printf("Video DONE: %v %v", t.Name(), video.ID)

		// Save videos to file after processing
		if err := saveVideosToFile(videos); err != nil {
			log.Printf("Error saving videos to file: %v", err)
		}
	}
}

func processVideoDirectory(videoDir, hlsDir string) (inputFile, outputFile string) {
	// Iterate through files in the directory and find a video file
	files, err := os.ReadDir(videoDir)
	if err != nil {
		log.Printf("Error reading directory: %v", err)
		return "", ""
	}

	var videoFile string
	for _, file := range files {
		if isVideoFile(file.Name()) {
			videoFile = file.Name()
			break
		}
	}

	if videoFile == "" {
		log.Printf("No video file found in the directory.")
		return "", ""
	}

	inputFile = filepath.Join(videoDir, videoFile)
	outputFile = filepath.Join(hlsDir, "output.m3u8")

	return inputFile, outputFile
}

func isVideoFile(filename string) bool {
	// You can customize this function based on the types of video files you want to support
	// For simplicity, this example checks if the file has a common video file extension.
	videoExtensions := []string{".mp4", ".mkv", ".avi", ".mov"}
	ext := strings.ToLower(filepath.Ext(filename))
	for _, validExt := range videoExtensions {
		if ext == validExt {
			return true
		}
	}
	return false
}

func createDirsIfNotExist(dirPaths []string) error {
	for _, dirPath := range dirPaths {
		// Check if the directory already exists
		_, err := os.Stat(dirPath)
		if os.IsNotExist(err) {
			// Directory doesn't exist, so create it
			err := os.MkdirAll(dirPath, os.ModePerm)
			if err != nil {
				return fmt.Errorf("error creating directory '%s': %v", dirPath, err)
			}
			fmt.Printf("Directory '%s' created successfully.\n", dirPath)
		} else if err != nil {
			// Some other error occurred
			return fmt.Errorf("error checking directory '%s': %v", dirPath, err)
		} else {
			fmt.Printf("Directory '%s' already exists.\n", dirPath)
		}
	}

	return nil
}

func main() {
	dirPaths := []string{"tmp", "tmp/hls", "tmp/torrent_data"}
	createDirsIfNotExist(dirPaths)

	// for _, video := range videos {
	// 	resumeDownloadAndProcess(video)
	// }

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/submit", submitHandler)
	fileHandler := http.StripPrefix("/stream/", http.FileServer(http.Dir("tmp/hls")))
	http.Handle("/stream/", fileHandler)

	log.Fatal(http.ListenAndServe(":8080", nil))
}
