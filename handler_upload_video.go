package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	uploadLimit := http.MaxBytesReader(w, r.Body, 1<<30) // 1GB
	defer uploadLimit.Close()

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse video uuid", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userId, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}

	if video.UserID != userId {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Incorrect file type, expected video/mp4", err)
		return
	}

	temp, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save file", err)
		return
	}

	defer os.Remove(temp.Name())

	defer temp.Close()

	if _, err := io.Copy(temp, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}

	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	randBytes := make([]byte, 16)
	_, err = rand.Read(randBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate filename", err)
		return
	}

	ratio, err := getVideoAspectRatio(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get the videos aspect ratio", err)
		return
	}

	var prefix string
	switch ratio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	fastStart, err := processVideoForFastStart(temp.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't move flag to beggining", err)
		return
	}
	defer os.Remove(fastStart)

	processedFile, err := os.Open(fastStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}
	defer processedFile.Close()

	key := fmt.Sprintf("%s/%x.mp4", prefix, randBytes)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't put object in bucket", err)
		return
	}

	s3URL := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, key)

	video.VideoURL = &s3URL

	log.Printf("Saving before VideoURL: %+v", video.VideoURL)
	err = cfg.db.UpdateVideo(video)
	log.Printf("Saving after VideoURL: %+v", video.VideoURL)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

type FFProbeOutput struct {
	Streams []Stream `json:"streams"`
}

type Stream struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var output bytes.Buffer
	cmd.Stdout = &output

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	var ffprobeData FFProbeOutput
	if err := json.Unmarshal(output.Bytes(), &ffprobeData); err != nil {
		return "", fmt.Errorf("failed to unmarshal json: %w", err)
	}

	if len(ffprobeData.Streams) == 0 {
		return "", fmt.Errorf("no streams found in video")
	}

	width := ffprobeData.Streams[0].Width
	height := ffprobeData.Streams[0].Height

	ratio := float64(width) / float64(height)

	const tolerance = 0.1

	if ratio > (16.0/9.0-tolerance) && ratio < (16.0/9.0+tolerance) {
		return "16:9", nil
	} else if ratio > (9.0/16.0-tolerance) && ratio < (9.0/16.0+tolerance) {
		return "9:16", nil
	} else {
		return "other", nil
	}

}

func processVideoForFastStart(filePath string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}
	output := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", output)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(output)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return output, nil
}
