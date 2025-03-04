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
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
)

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

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	var uploadLimit int64 = 1 << 30
	http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload this video", err)
	}

	file, header, err := r.FormFile("video")
	defer file.Close()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get file", err)
		return
	}
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusUnsupportedMediaType, "Unsupported media type", err)
		return
	}
	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	n, err := io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	} else {
		log.Printf("Copied %v bytes", n)
	}
	tmpFile.Seek(0, io.SeekStart)

	aspectRatio, err := getVideoAspectRatio(tmpFile.Name())

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	filename := fmt.Sprintf("%s/%x.mp4", aspectRatio, key)
	params := &s3.PutObjectInput{
		Bucket:        aws.String(cfg.s3Bucket),
		Key:           aws.String(filename),
		ContentType:   aws.String(mediaType),
		ContentLength: aws.Int64(n),
		Body:          tmpFile,
	}
	_, err = cfg.s3Client.PutObject(context.TODO(), params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video", err)
		return
	}
	videoUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, filename)
	video.VideoURL = &videoUrl
	if err = cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video to database", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
	return
}
