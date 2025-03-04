package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
)

// handlerUploadVideo provides a handler for video uploads. It retrieves the
// ID of the video from the URL and the user from the token, validates that
// the user has permission to upload the video, processes the video for fast
// start and uploads the processed file to s3 storage for later retrieval.
func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	var uploadLimit int64 = 1 << 30
	http.MaxBytesReader(w, r.Body, uploadLimit)

	// Get video ID from URL
	videoID, err := getVideoID(r.PathValue("videoID"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// Get user ID from auth token
	userID, err := getUserID(cfg, r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Invalid user ID", err)
		return
	}

	// Get video from the database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
	}

	// Verify ownership of the video
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload this video", err)
	}

	// Get the uploaded video file
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get file", err)
		return
	}
	defer file.Close()

	// Validate the content type of the upload
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported media type", err)
		return
	}

	// Create a temporary file for processing
	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Copy the upload to the temp file and rewind for processing
	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}
	tmpFile.Seek(0, io.SeekStart)

	// Get the aspect ratio of the video
	aspectRatio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get aspect ratio", err)
	}

	processedFilename, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
	}

	body, err := os.Open(processedFilename)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open fast start processed file", err)
	}
	defer os.Remove(processedFilename)
	defer body.Close()

	// Upload the video to S3
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	filename := fmt.Sprintf("%s/%x.mp4", aspectRatio, key)
	params := &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(filename),
		ContentType: aws.String(mediaType),
		Body:        body,
	}
	_, err = cfg.s3Client.PutObject(context.TODO(), params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video", err)
		return
	}

	// Store the video metadata in the database
	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, filename)
	video.VideoURL = &videoUrl
	if err = cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video to database", err)
		return
	}

	// Write the success response
	respondWithJSON(w, http.StatusOK, video)
}

// processVideoForFastStart uses ffmpeg to re-order the metadata in the video
// using ffmpeg so that the movflags appear at the beginning of the file,
// removing the need for two requests to preload the video content in the
// browser.
func processVideoForFastStart(filepath string) (string, error) {
	outputFilepath := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilepath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFilepath, nil
}

// getVideoAspectRatio retrieves the video's aspect ratio from the metadata
// using ffprobe.
func getVideoAspectRatio(filePath string) (string, error) {
	type videoData struct {
		Streams []struct {
			Width              int    `json:"width"`
			Height             int    `json:"height"`
			DisplayAspectRatio string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := bytes.NewBuffer(nil)
	cmd.Stdout = buffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var data videoData
	err = json.Unmarshal(buffer.Bytes(), &data)
	if err != nil {
		return "", err
	}

	ratio := data.Streams[0].DisplayAspectRatio
	if ratio == "16:9" {
		return "landscape", nil
	} else if ratio == "9:16" {
		return "portrait", nil
	} else {
		return "other", nil
	}
}

// getVideoID converts the parameter to a UUID
func getVideoID(param string) (uuid.UUID, error) {
	videoID, err := uuid.Parse(param)
	if err != nil {
		return uuid.Nil, err
	}
	return videoID, nil
}

// getUserID retrieves the user ID from the auth token header
func getUserID(cfg *apiConfig, header http.Header) (uuid.UUID, error) {
	token, err := auth.GetBearerToken(header)
	if err != nil {
		return uuid.Nil, err
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		return uuid.Nil, err
	}

	return userID, nil
}
