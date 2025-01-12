package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	apiKey     string
	httpClient = &http.Client{}
	bufferPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	allowedOrigins               []string
	enableTranscription          bool
	transcriptionProvider        string
	openaiAPIKey                 string
	groqAPIKey                   string
	defaultTranscriptionLanguage string
	enableS3Storage              bool
	s3Endpoint                   string
	s3AccessKey                  string
	s3SecretKey                  string
	s3BucketName                 string
	s3Region                     string
	s3UseSSL                     bool
	s3Client                     *minio.Client
	s3URLExpiration              time.Duration
)

func init() {
	devMode := flag.Bool("dev", false, "Run in development mode")
	flag.Parse()

	if *devMode {
		err := godotenv.Load()
		if err != nil {
			fmt.Println("Error loading .env file")
		} else {
			fmt.Println(".env file loaded successfully")
		}
	}

	apiKey = os.Getenv("API_KEY")
	if apiKey == "" {
		fmt.Println("API_KEY not configured in .env file")
	}

	allowOriginsEnv := os.Getenv("CORS_ALLOW_ORIGINS")
	if allowOriginsEnv != "" {
		allowedOrigins = strings.Split(allowOriginsEnv, ",")
		fmt.Printf("Allowed origins: %v\n", allowedOrigins)
	} else {
		allowedOrigins = []string{"*"}
		fmt.Println("No specific origins configured, allowing all (*)")
	}

	enableTranscription = os.Getenv("ENABLE_TRANSCRIPTION") == "true"
	transcriptionProvider = os.Getenv("TRANSCRIPTION_PROVIDER")
	openaiAPIKey = os.Getenv("OPENAI_API_KEY")
	groqAPIKey = os.Getenv("GROQ_API_KEY")
	defaultTranscriptionLanguage = os.Getenv("TRANSCRIPTION_LANGUAGE")

	// Configuração do S3
	enableS3Storage = os.Getenv("ENABLE_S3_STORAGE") == "true"
	if enableS3Storage {
		s3Endpoint = os.Getenv("S3_ENDPOINT")
		s3AccessKey = os.Getenv("S3_ACCESS_KEY")
		s3SecretKey = os.Getenv("S3_SECRET_KEY")
		s3BucketName = os.Getenv("S3_BUCKET_NAME")
		s3Region = os.Getenv("S3_REGION")
		s3UseSSL = os.Getenv("S3_USE_SSL") == "true"

		// Parse URL expiration duration, default to 24 hours
		expiration := os.Getenv("S3_URL_EXPIRATION")
		if expiration == "" {
			expiration = "24h"
		}
		var err error
		s3URLExpiration, err = time.ParseDuration(expiration)
		if err != nil {
			fmt.Printf("Invalid S3_URL_EXPIRATION format, using default 24h: %v\n", err)
			s3URLExpiration = 24 * time.Hour
		}

		// Initialize MinIO client
		minioClient, err := minio.New(s3Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(s3AccessKey, s3SecretKey, ""),
			Secure: s3UseSSL,
			Region: s3Region,
		})
		if err != nil {
			fmt.Printf("Error initializing S3 client: %v\n", err)
			return
		}
		s3Client = minioClient

		// Create bucket if it doesn't exist
		exists, err := s3Client.BucketExists(context.Background(), s3BucketName)
		if err != nil {
			fmt.Printf("Error checking bucket existence: %v\n", err)
			return
		}

		if !exists {
			err = s3Client.MakeBucket(context.Background(), s3BucketName, minio.MakeBucketOptions{Region: s3Region})
			if err != nil {
				fmt.Printf("Error creating bucket: %v\n", err)
				return
			}
			fmt.Printf("Created bucket: %s\n", s3BucketName)
		}
	}
}

func validateAPIKey(c *gin.Context) bool {
	if apiKey == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return false
	}

	requestApiKey := c.GetHeader("apikey")
	if requestApiKey == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API_KEY not provided"})
		return false
	}

	if requestApiKey != apiKey {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API_KEY"})
		return false
	}

	return true
}

func convertAudio(inputData []byte, format string) ([]byte, int, error) {
	var cmd *exec.Cmd
	switch format {
	case "mp3":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-f", "mp3", "pipe:1")
	case "mp4":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-c:a", "aac", "-f", "mp4", "pipe:1")
	default:
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-ac", "1", "-ar", "16000", "-c:a", "libopus", "-f", "ogg", "pipe:1")
	}
	outBuffer := bufferPool.Get().(*bytes.Buffer)
	errBuffer := bufferPool.Get().(*bytes.Buffer)
	defer bufferPool.Put(outBuffer)
	defer bufferPool.Put(errBuffer)

	outBuffer.Reset()
	errBuffer.Reset()

	cmd.Stdin = bytes.NewReader(inputData)
	cmd.Stdout = outBuffer
	cmd.Stderr = errBuffer

	err := cmd.Run()
	if err != nil {
		return nil, 0, fmt.Errorf("error during conversion: %v, details: %s", err, errBuffer.String())
	}

	convertedData := make([]byte, outBuffer.Len())
	copy(convertedData, outBuffer.Bytes())

	// Parsing da duração
	outputText := errBuffer.String()
	splitTime := strings.Split(outputText, "time=")

	if len(splitTime) < 2 {
		return nil, 0, errors.New("duração não encontrada")
	}

	re := regexp.MustCompile(`(\d+):(\d+):(\d+\.\d+)`)
	matches := re.FindStringSubmatch(splitTime[2])
	if len(matches) != 4 {
		return nil, 0, errors.New("formato de duração não encontrado")
	}

	hours, _ := strconv.ParseFloat(matches[1], 64)
	minutes, _ := strconv.ParseFloat(matches[2], 64)
	seconds, _ := strconv.ParseFloat(matches[3], 64)
	duration := int(hours*3600 + minutes*60 + seconds)

	return convertedData, duration, nil
}

func fetchAudioFromURL(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func getInputData(c *gin.Context) ([]byte, error) {
	if file, _, err := c.Request.FormFile("file"); err == nil {
		return io.ReadAll(file)
	}

	if base64Data := c.PostForm("base64"); base64Data != "" {
		return base64.StdEncoding.DecodeString(base64Data)
	}

	if url := c.PostForm("url"); url != "" {
		return fetchAudioFromURL(url)
	}

	return nil, errors.New("no file, base64 or URL provided")
}

func transcribeAudio(audioData []byte, language string) (string, error) {
	if !enableTranscription {
		return "", errors.New("transcription is not enabled")
	}

	// Se nenhum idioma foi especificado, use o padrão do .env
	if language == "" {
		language = defaultTranscriptionLanguage
	}

	switch transcriptionProvider {
	case "openai":
		return transcribeWithOpenAI(audioData, language)
	case "groq":
		return transcribeWithGroq(audioData, language)
	default:
		return "", errors.New("invalid transcription provider")
	}
}

func transcribeWithOpenAI(audioData []byte, language string) (string, error) {
	if openaiAPIKey == "" {
		return "", errors.New("OpenAI API key not configured")
	}

	// Se nenhum idioma foi especificado, use o padrão
	if language == "" {
		language = defaultTranscriptionLanguage
	}

	// Salvar temporariamente o arquivo
	tempFile, err := os.CreateTemp("", "audio-*.ogg")
	if err != nil {
		return "", err
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.Write(audioData); err != nil {
		return "", err
	}
	tempFile.Close()

	url := "https://api.openai.com/v1/audio/transcriptions"
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Adicionar o arquivo
	file, err := os.Open(tempFile.Name())
	if err != nil {
		return "", err
	}
	defer file.Close()

	part, err := writer.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", err
	}
	io.Copy(part, file)

	// Adicionar modelo e idioma
	writer.WriteField("model", "whisper-1")
	if language != "" {
		writer.WriteField("language", language)
	}

	writer.Close()

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+openaiAPIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("erro na API OpenAI (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Text, nil
}

func transcribeWithGroq(audioData []byte, language string) (string, error) {
	if groqAPIKey == "" {
		return "", errors.New("Groq API key not configured")
	}

	// Se nenhum idioma foi especificado, use o padrão
	if language == "" {
		language = defaultTranscriptionLanguage
	}

	// Salvar temporariamente o arquivo
	tempFile, err := os.CreateTemp("", "audio-*.ogg")
	if err != nil {
		return "", err
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.Write(audioData); err != nil {
		return "", err
	}
	tempFile.Close()

	url := "https://api.groq.com/openai/v1/audio/transcriptions"
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Adicionar o arquivo
	file, err := os.Open(tempFile.Name())
	if err != nil {
		return "", err
	}
	defer file.Close()

	part, err := writer.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", err
	}
	io.Copy(part, file)

	// Adicionar modelo e configurações
	writer.WriteField("model", "whisper-large-v3-turbo") // modelo mais rápido e com bom custo-benefício
	if language != "" {
		writer.WriteField("language", language)
	}
	writer.WriteField("response_format", "json")
	writer.WriteField("temperature", "0.0") // mais preciso

	writer.Close()

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+groqAPIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("erro na API Groq (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Text, nil
}

func uploadToS3(data []byte, format string) (string, error) {
	if !enableS3Storage || s3Client == nil {
		return "", errors.New("S3 storage is not enabled or properly configured")
	}

	// Generate unique filename
	filename := fmt.Sprintf("%d.%s", time.Now().UnixNano(), format)
	contentType := fmt.Sprintf("audio/%s", format)

	// Upload to S3
	_, err := s3Client.PutObject(
		context.Background(),
		s3BucketName,
		filename,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType},
	)
	if err != nil {
		return "", fmt.Errorf("error uploading to S3: %v", err)
	}

	// Generate presigned URL
	url, err := s3Client.PresignedGetObject(
		context.Background(),
		s3BucketName,
		filename,
		s3URLExpiration,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("error generating presigned URL: %v", err)
	}

	return url.String(), nil
}

func processAudio(c *gin.Context) {
	if !validateAPIKey(c) {
		return
	}

	inputData, err := getInputData(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	format := c.DefaultPostForm("format", "ogg")

	convertedData, duration, err := convertAudio(inputData, format)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	response := gin.H{
		"duration": duration,
		"format":   format,
	}

	// Handle S3 upload if enabled
	if enableS3Storage {
		url, err := uploadToS3(convertedData, format)
		if err != nil {
			fmt.Printf("Error uploading to S3: %v\n", err)
			// Fallback to base64 if S3 upload fails
			response["audio"] = base64.StdEncoding.EncodeToString(convertedData)
		} else {
			response["url"] = url
		}
	} else {
		response["audio"] = base64.StdEncoding.EncodeToString(convertedData)
	}

	// Handle transcription if requested
	if c.DefaultPostForm("transcribe", "false") == "true" {
		language := c.DefaultPostForm("language", "")
		transcription, err := transcribeAudio(convertedData, language)
		if err != nil {
			fmt.Printf("Error in transcription: %v\n", err)
		} else {
			response["transcription"] = transcription
		}
	}

	c.JSON(http.StatusOK, response)
}

func validateOrigin(origin string) bool {
	fmt.Printf("Validating origin: %s\n", origin)
	fmt.Printf("Allowed origins: %v\n", allowedOrigins)

	if len(allowedOrigins) == 0 {
		return true
	}

	if origin == "" {
		return true
	}

	for _, allowed := range allowedOrigins {
		allowed = strings.TrimSpace(allowed)

		if allowed == "*" {
			return true
		}

		if allowed == origin {
			fmt.Printf("Origin %s matches %s\n", origin, allowed)
			return true
		}
	}

	fmt.Printf("Origin %s not found in allowed origins\n", origin)
	return false
}

func originMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		fmt.Printf("\n=== CORS Debug ===\n")
		fmt.Printf("Received origin: %s\n", origin)
		fmt.Printf("Complete headers: %+v\n", c.Request.Header)
		fmt.Printf("Allowed origins: %v\n", allowedOrigins)
		fmt.Printf("=================\n")

		if origin == "" {
			origin = c.Request.Header.Get("Referer")
			fmt.Printf("Empty origin, using Referer: %s\n", origin)
		}

		if !validateOrigin(origin) {
			fmt.Printf("❌ Origin rejected: %s\n", origin)
			c.JSON(http.StatusForbidden, gin.H{"error": "Origin not allowed"})
			c.Abort()
			return
		}

		fmt.Printf("✅ Origin accepted: %s\n", origin)
		c.Next()
	}
}

func transcribeOnly(c *gin.Context) {
	if !validateAPIKey(c) {
		return
	}

	inputData, err := getInputData(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Converter para ogg primeiro
	convertedData, _, err := convertAudio(inputData, "ogg")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Pega o idioma da requisição ou usa vazio para usar o padrão do .env
	language := c.DefaultPostForm("language", "")
	transcription, err := transcribeAudio(convertedData, language)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"transcription": transcription,
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	router := gin.Default()

	config := cors.DefaultConfig()
	config.AllowOrigins = allowedOrigins
	config.AllowMethods = []string{"POST", "GET", "OPTIONS"}
	config.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization", "apikey"}
	config.AllowCredentials = true

	router.Use(cors.New(config))
	router.Use(originMiddleware())

	router.POST("/process-audio", processAudio)
	router.POST("/transcribe", transcribeOnly)

	router.Run(":" + port)
}
