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
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/gorilla/websocket"
)

var (
	upgrader      = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	torrentClient *torrent.Client
	torrentMutex  sync.Mutex
	videoMutex    sync.Mutex
	videos        map[string]*Video
)

// Video struct represents a video being processed or ready to stream.
type Video struct {
	ID               string
	Name             string
	Status           string
	DownloadProgress float64 // Download progress (percentage)
	ProcessingStatus string  // Processing status ("In Progress" or "Done")
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
	tmpl, err := template.New("index").Parse(`
	<!DOCTYPE html>
		<html>

		<head>
			<title>Torrent Streamer</title>
			<!-- Include hls.js library -->
			<script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script>
		</head>

		<body>
			<h1>Torrent Streamer</h1>
			<form action="/submit" method="post">
				<label for="magnetLink">Magnet Link:</label>
				<input type="text" id="magnetLink" name="magnetLink" required>
				<button type="submit">Submit</button>
			</form>

			<h2>Downloading Videos:</h2>
			<ul id="downloadingList"></ul>

			<h2>Processing Videos:</h2>
			<ul id="processingList"></ul>

			<h2>Ready Videos:</h2>
			<ul id="readyList"></ul>

			<script>
				var oldData = null;
				var ws = new WebSocket("ws://" + window.location.host + "/ws");
				ws.onmessage = function (event) {
					var data = JSON.parse(event.data);
					if (oldData === event.data) {
						return
					}
					oldData = event.data;
					updateStatus(data);
				};

				function updateStatus(data) {
					console.log(data);

					if (typeof data !== 'object' || data === null || Array.isArray(data)) {
						console.error("Invalid data format");
						return;
					}

					var downloadingVideos = [];
					var processingVideos = [];
					var readyVideos = [];

					for (var key in data) {
						if (data.hasOwnProperty(key)) {
							var video = data[key];
							if (video.Status === "Downloading") {
								downloadingVideos.push(video);
							} else if (video.Status === "Processing") {
								processingVideos.push(video);
							} else if (video.Status === "Ready") {
								readyVideos.push(video);
							}
						}
					}

					updateList("downloadingList", downloadingVideos, "Download Progress: ");
					updateList("processingList", processingVideos, "Processing Status: ");
					updateList("readyList", readyVideos, "Watch Stream");
				}

				function updateList(listId, videos, statusPrefix) {
					var videoList = document.getElementById(listId);
					videoList.innerHTML = "";

					videos.forEach(function (video) {
						var listItem = document.createElement("li");
						listItem.innerHTML = video.Name + " - " + getStatusText(video, statusPrefix);
						videoList.appendChild(listItem);

						if (statusPrefix === "Watch Stream") {
							// For ready videos, add an hls.js player
							var br = document.createElement("br");
							var videoPlayer = document.createElement("video");
							videoPlayer.setAttribute("controls", "");
							videoPlayer.setAttribute("preload", "auto");
							videoPlayer.setAttribute("width", "640");
							videoPlayer.setAttribute("height", "264");

							var videoSource = document.createElement("source");
							videoSource.setAttribute("src", "/stream/" + video.ID + "/output.m3u8");
							videoSource.setAttribute("type", "application/x-mpegURL");

							videoPlayer.appendChild(videoSource);
							listItem.appendChild(br);
							listItem.appendChild(videoPlayer);

							// Initialize hls.js
							var hls = new Hls();
							hls.loadSource("/stream/" + video.ID + "/output.m3u8");
							hls.attachMedia(videoPlayer);
						}
					});
				}

				function getStatusText(video, statusPrefix) {
					if (video.Status === "Downloading") {
						return "Download Progress: " + video.DownloadProgress.toFixed(2) + "%";
					} else if (video.Status === "Processing") {
						return "Processing Status: " + video.ProcessingStatus;
					} else if (video.Status === "Ready") {
						return '<a href="/stream/' + video.ID + '/output.m3u8" target="_blank">' + statusPrefix + '</a>';
					} else {
						return "Unknown Status";
					}
				}
			</script>
		</body>

		</html>
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, videos)
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
		videoMutex.Lock()
		videoData, err := json.Marshal(videos)
		videoMutex.Unlock()

		if err != nil {
			log.Printf("Error encoding video data: %v", err)
			continue
		}

		// Send video information to WebSocket
		err = conn.WriteMessage(websocket.TextMessage, videoData)
		if err != nil {
			log.Printf("Error sending message to WebSocket: %v", err)
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

	torrentMutex.Lock()
	log.Println(magnetLink)
	t, err := torrentClient.AddMagnet(magnetLink)
	<-t.GotInfo()
	log.Println(t.Name())
	torrentMutex.Unlock()

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	videoID := fmt.Sprintf("%x", t.InfoHash())
	videoName := filepath.Base(t.Name())
	video := Video{ID: videoID, Name: videoName, Status: "Downloading", DownloadProgress: 0}

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

	// Download torrent
	torrentMutex.Lock()
	t.DownloadAll()
	torrentMutex.Unlock()

	select {
	case <-t.Complete.On():
		// Wait for the download to complete
		ticker.Stop()

		video.Status = "Processing"
		video.DownloadProgress = 100 // Set download progress to 100% after download is complete
		video.ProcessingStatus = "In Progress"
		videoMutex.Lock()

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
			"-hls_time", "20",
			"-hls_playlist_type", "vod",
			"-hls_segment_type", "mpegts",
			"-hls_flags", "independent_segments",
			"-hls_list_size", "0",
			"-hls_segment_filename", filepath.Join(hlsDir, "segment%d.ts"),
			outputFile,
		)
		err := cmd.Run()
		videoMutex.Unlock()
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

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/submit", submitHandler)
	fileHandler := http.StripPrefix("/stream/", http.FileServer(http.Dir("tmp/hls")))
	http.Handle("/stream/", fileHandler)

	log.Fatal(http.ListenAndServe(":8080", nil))
}
