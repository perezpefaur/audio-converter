package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

var (
	apiKey     string
	httpClient = &http.Client{}
	bufferPool = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	allowedOrigins []string
)

func init() {
	devMode := flag.Bool("dev", false, "Rodar em modo de desenvolvimento")
	flag.Parse()

	if *devMode {
		err := godotenv.Load()
		if err != nil {
			fmt.Println("Erro ao carregar o arquivo .env")
		} else {
			fmt.Println("Arquivo .env carregado com sucesso")
		}
	}

	apiKey = os.Getenv("API_KEY")
	if apiKey == "" {
		fmt.Println("API_KEY não configurada no arquivo .env")
	}

	allowOriginsEnv := os.Getenv("CORS_ALLOW_ORIGINS")
	if allowOriginsEnv != "" {
		allowedOrigins = strings.Split(allowOriginsEnv, ",")
	} else {
		allowedOrigins = []string{"*"}
	}
}

func validateAPIKey(c *gin.Context) bool {
	if apiKey == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Erro interno no servidor"})
		return false
	}

	requestApiKey := c.GetHeader("apikey")
	if requestApiKey == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API_KEY não fornecida"})
		return false
	}

	if requestApiKey != apiKey {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "API_KEY inválida"})
		return false
	}

	return true
}

func convertAudio(inputData []byte, inputFormat string, outputFormat string) ([]byte, int, error) {
	var cmd *exec.Cmd
	switch outputFormat {
	case "mp3":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-f", "mp3", "pipe:1")
	case "wav":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-f", "wav", "pipe:1")
	case "aac":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-c:a", "aac", "-b:a", "128k", "-f", "adts", "pipe:1")
	case "amr":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-c:a", "libopencore_amrnb", "-b:a", "12.2k", "-f", "amr", "pipe:1")
	case "m4a":
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-c:a", "aac", "-b:a", "128k", "-f", "ipod", "pipe:1")
	default:
		cmd = exec.Command("ffmpeg", "-i", "pipe:0", "-c:a", "libopus", "-b:a", "16k", "-vbr", "on", "-compression_level", "10", "-ac", "1", "-ar", "16000", "-f", "ogg", "pipe:1")
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

func fetchGifFromURL(url string) ([]byte, error) {
	if url == "" {
		return nil, errors.New("URL vazia fornecida")
	}

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("erro ao acessar URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status de resposta inválido: %d", resp.StatusCode)
	}

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

	return nil, errors.New("nenhum arquivo, base64 ou URL fornecido")
}

func convertGifToMp4(inputData []byte) ([]byte, error) {
	cmd := exec.Command("ffmpeg", "-i", "pipe:0", "-movflags", "faststart", "-pix_fmt", "yuv420p", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-f", "mp4", "pipe:1")

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
		return nil, fmt.Errorf("error during conversion: %v, details: %s", err, errBuffer.String())
	}

	convertedData := make([]byte, outBuffer.Len())
	copy(convertedData, outBuffer.Bytes())

	return convertedData, nil
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

	outputFormat := c.DefaultPostForm("output_format", "ogg")
	inputFormat := c.DefaultPostForm("input_format", "ogg")

	convertedData, duration, err := convertAudio(inputData, inputFormat, outputFormat)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"duration": duration,
		"audio":    base64.StdEncoding.EncodeToString(convertedData),
		"format":   outputFormat,
	})
}

func processGifToMp4(c *gin.Context) {
	if !validateAPIKey(c) {
		return
	}

	// Verificar si hay una URL en el formulario
	if url := c.PostForm("url"); url != "" {
		inputData, err := fetchGifFromURL(url)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		convertedData, err := convertGifToMp4(inputData)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"video": base64.StdEncoding.EncodeToString(convertedData),
			"format": "mp4",
		})
		return
	}

	// Si no hay URL, intentar otros métodos de entrada
	inputData, err := getInputData(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	convertedData, err := convertGifToMp4(inputData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"video": base64.StdEncoding.EncodeToString(convertedData),
		"format": "mp4",
	})
}

func validateOrigin(origin string) bool {
	if len(allowedOrigins) == 0 || (len(allowedOrigins) == 1 && allowedOrigins[0] == "*") {
		return true
	}

	for _, allowed := range allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

func originMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			origin = c.Request.Header.Get("Referer")
		}

		if !validateOrigin(origin) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Origem não permitida"})
			c.Abort()
			return
		}
		c.Next()
	}
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

	router.Use(cors.New(config))
	router.Use(originMiddleware())

	router.POST("/process-audio", processAudio)
	router.POST("/gif-to-mp4", processGifToMp4)

	router.Run(":" + port)
}
