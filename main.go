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
	"time"

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

	fmt.Printf("Intentando descargar GIF desde: %s\n", url)

	// Configurar un cliente HTTP con timeout más largo
	client := &http.Client{
		Timeout: 60 * time.Second, // Aumentar timeout a 60 segundos
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error al crear solicitud: %v", err)
	}

	// Agregar User-Agent para evitar restricciones
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error al acceder URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("estado de respuesta inválido: %d", resp.StatusCode)
	}

	fmt.Printf("Descarga iniciada. Content-Length: %s\n", resp.Header.Get("Content-Length"))

	// Leer con un buffer limitado para evitar problemas de memoria
	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error al leer datos: %v", err)
	}

	data := buffer.Bytes()
	fmt.Printf("Descarga completada. Tamaño: %d bytes\n", len(data))

	return data, nil
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
	// Log the size of the input data
	fmt.Printf("Tamaño de datos GIF de entrada: %d bytes\n", len(inputData))

	// Verificar que los datos de entrada no estén vacíos
	if len(inputData) == 0 {
		return nil, errors.New("datos de entrada vacíos")
	}

	// Guardar los primeros bytes para verificar el formato
	headerBytes := 16
	if len(inputData) < headerBytes {
		headerBytes = len(inputData)
	}
	fmt.Printf("Primeros %d bytes: %v\n", headerBytes, inputData[:headerBytes])

	// Siempre usar archivos temporales para MP4 porque el formato requiere seeking
	// que no es posible con pipes
	return convertGifToMp4UsingTempFiles(inputData)
}

// Función para convertir GIF a MP4 usando archivos temporales
func convertGifToMp4UsingTempFiles(inputData []byte) ([]byte, error) {
	fmt.Println("Usando archivos temporales para la conversión de GIF a MP4")

	// Crear archivo temporal para entrada
	inputFile, err := os.CreateTemp("", "input-*.gif")
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de entrada: %v", err)
	}
	inputPath := inputFile.Name()
	defer func() {
		inputFile.Close()
		os.Remove(inputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de entrada eliminado: %s\n", inputPath)
	}()

	// Escribir datos de entrada al archivo temporal
	bytesWritten, err := inputFile.Write(inputData)
	if err != nil {
		return nil, fmt.Errorf("error al escribir en archivo temporal: %v", err)
	}
	fmt.Printf("Datos escritos en archivo temporal: %d bytes en %s\n", bytesWritten, inputPath)
	inputFile.Close() // Cerrar archivo después de escribir

	// Crear archivo temporal para salida
	outputFile, err := os.CreateTemp("", "output-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de salida: %v", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close() // Cerrar para que ffmpeg pueda escribir en él
	defer func() {
		os.Remove(outputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de salida eliminado: %s\n", outputPath)
	}()

	// Verificar que el archivo de entrada existe y tiene tamaño
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de entrada: %v", err)
	}
	fmt.Printf("Archivo de entrada verificado: %s (tamaño: %d bytes)\n", inputPath, inputInfo.Size())

	// Ejecutar ffmpeg con archivos temporales
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,          // Archivo de entrada
		"-movflags", "faststart", // Optimizar para streaming
		"-pix_fmt", "yuv420p",    // Formato de pixel compatible
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", // Asegurar dimensiones pares
		"-f", "mp4",              // Formato de salida
		"-c:v", "libx264",        // Codec de video
		"-preset", "ultrafast",   // Preset de codificación más rápido
		"-crf", "23",             // Calidad de video
		"-y",                     // Sobrescribir sin preguntar
		outputPath)               // Archivo de salida

	// Capturar salida de error
	var errBuffer bytes.Buffer
	cmd.Stderr = &errBuffer

	fmt.Println("Ejecutando FFmpeg con archivos temporales...")
	fmt.Printf("Comando: %v\n", cmd.Args)

	err = cmd.Run()
	if err != nil {
		fmt.Printf("Error durante la conversión con archivos temporales: %v\n", err)
		fmt.Printf("Detalles del error: %s\n", errBuffer.String())
		return nil, fmt.Errorf("error en conversión con archivos temporales: %v, detalles: %s", err, errBuffer.String())
	}

	// Verificar que el archivo de salida existe y tiene tamaño
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de salida: %v", err)
	}
	fmt.Printf("Archivo de salida verificado: %s (tamaño: %d bytes)\n", outputPath, outputInfo.Size())

	// Leer archivo de salida
	outputData, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al leer archivo de salida: %v", err)
	}

	if len(outputData) == 0 {
		return nil, errors.New("la conversión produjo un archivo de salida vacío")
	}

	fmt.Printf("Conversión con archivos temporales exitosa. Tamaño del MP4: %d bytes\n", len(outputData))
	return outputData, nil
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
	// Función para manejar errores y responder al cliente
	handleError := func(statusCode int, err error, source string) {
		errorMsg := err.Error()
		fmt.Printf("Error en %s: %v\n", source, err)
		c.JSON(statusCode, gin.H{"error": errorMsg})
	}

	// Función para procesar la conversión y responder al cliente
	processConversion := func(inputData []byte, source string) {
		fmt.Printf("Procesando GIF desde %s (%d bytes)\n", source, len(inputData))

		// Implementar recuperación de pánico
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("Recuperado de pánico en conversión: %v\n", r)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": fmt.Sprintf("Error interno durante la conversión: %v", r),
				})
			}
		}()

		convertedData, err := convertGifToMp4(inputData)
		if err != nil {
			handleError(http.StatusInternalServerError, err, "conversión")
			return
		}

		// Verificar que los datos convertidos no estén vacíos
		if len(convertedData) == 0 {
			handleError(http.StatusInternalServerError,
				errors.New("la conversión produjo un archivo vacío"), "validación de salida")
			return
		}

		fmt.Printf("Conversión exitosa. Enviando respuesta (%d bytes)\n", len(convertedData))
		c.JSON(http.StatusOK, gin.H{
			"video": base64.StdEncoding.EncodeToString(convertedData),
			"format": "mp4",
		})
	}

	// Validar API Key
	if !validateAPIKey(c) {
		return
	}

	// Log para depuración
	fmt.Printf("Recibida solicitud GIF a MP4. Content-Type: %s\n", c.ContentType())

	// Verificar si hay una URL en el formulario
	formUrl := c.PostForm("url")
	if formUrl != "" {
		fmt.Printf("URL encontrada en form-data: %s\n", formUrl)
		inputData, err := fetchGifFromURL(formUrl)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de GIF (form)")
			return
		}
		processConversion(inputData, "form-data")
		return
	}

	// Verificar si hay una URL en los parámetros de consulta
	queryUrl := c.Query("url")
	if queryUrl != "" {
		fmt.Printf("URL encontrada en query params: %s\n", queryUrl)
		inputData, err := fetchGifFromURL(queryUrl)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de GIF (query)")
			return
		}
		processConversion(inputData, "query params")
		return
	}

	// Verificar si hay datos en JSON
	var jsonData struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&jsonData); err == nil && jsonData.URL != "" {
		fmt.Printf("URL encontrada en JSON: %s\n", jsonData.URL)
		inputData, err := fetchGifFromURL(jsonData.URL)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de GIF (json)")
			return
		}
		processConversion(inputData, "JSON")
		return
	}

	// Si no hay URL, intentar otros métodos de entrada
	fmt.Println("No se encontró URL, intentando otros métodos de entrada")
	inputData, err := getInputData(c)
	if err != nil {
		handleError(http.StatusBadRequest, err, "obtención de datos de entrada")
		return
	}
	processConversion(inputData, "otros métodos")
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

func convertVideoToMp4(inputData []byte, inputFormat string) ([]byte, error) {
	fmt.Printf("Iniciando conversión de video %s a MP4 (%d bytes)\n", inputFormat, len(inputData))

	// Siempre usar archivos temporales para MP4 porque el formato requiere seeking
	// que no es posible con pipes
	return convertVideoToMp4UsingTempFiles(inputData, inputFormat)
}

// Función para convertir video a MP4 usando archivos temporales
func convertVideoToMp4UsingTempFiles(inputData []byte, inputFormat string) ([]byte, error) {
	fmt.Println("Usando archivos temporales para la conversión de video a MP4")

	// Crear archivo temporal para entrada
	inputFile, err := os.CreateTemp("", fmt.Sprintf("input-*.%s", inputFormat))
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de entrada: %v", err)
	}
	inputPath := inputFile.Name()
	defer func() {
		inputFile.Close()
		os.Remove(inputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de entrada eliminado: %s\n", inputPath)
	}()

	// Escribir datos de entrada al archivo temporal
	bytesWritten, err := inputFile.Write(inputData)
	if err != nil {
		return nil, fmt.Errorf("error al escribir en archivo temporal: %v", err)
	}
	fmt.Printf("Datos escritos en archivo temporal: %d bytes en %s\n", bytesWritten, inputPath)
	inputFile.Close() // Cerrar archivo después de escribir

	// Crear archivo temporal para salida
	outputFile, err := os.CreateTemp("", "output-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de salida: %v", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close() // Cerrar para que ffmpeg pueda escribir en él
	defer func() {
		os.Remove(outputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de salida eliminado: %s\n", outputPath)
	}()

	// Verificar que el archivo de entrada existe y tiene tamaño
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de entrada: %v", err)
	}
	fmt.Printf("Archivo de entrada verificado: %s (tamaño: %d bytes)\n", inputPath, inputInfo.Size())

	// Ejecutar ffmpeg con archivos temporales
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,          // Archivo de entrada
		"-movflags", "faststart", // Optimizar para streaming
		"-pix_fmt", "yuv420p",    // Formato de pixel compatible
		"-c:v", "libx264",        // Codec de video
		"-preset", "ultrafast",   // Preset de codificación más rápido
		"-crf", "23",             // Calidad de video
		"-c:a", "aac",            // Codec de audio
		"-b:a", "128k",           // Bitrate de audio
		"-y",                     // Sobrescribir sin preguntar
		outputPath)               // Archivo de salida

	// Capturar salida de error
	var errBuffer bytes.Buffer
	cmd.Stderr = &errBuffer

	fmt.Println("Ejecutando FFmpeg para conversión de video...")
	fmt.Printf("Comando: %v\n", cmd.Args)

	err = cmd.Run()
	if err != nil {
		fmt.Printf("Error durante la conversión de video: %v\n", err)
		fmt.Printf("Detalles del error: %s\n", errBuffer.String())
		return nil, fmt.Errorf("error en conversión de video: %v, detalles: %s", err, errBuffer.String())
	}

	// Verificar que el archivo de salida existe y tiene tamaño
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de salida: %v", err)
	}
	fmt.Printf("Archivo de salida verificado: %s (tamaño: %d bytes)\n", outputPath, outputInfo.Size())

	// Leer archivo de salida
	outputData, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al leer archivo de salida: %v", err)
	}

	if len(outputData) == 0 {
		return nil, errors.New("la conversión produjo un archivo de salida vacío")
	}

	fmt.Printf("Conversión de video exitosa. Tamaño del MP4: %d bytes\n", len(outputData))
	return outputData, nil
}

func processVideoToMp4(c *gin.Context) {
	// Función para manejar errores y responder al cliente
	handleError := func(statusCode int, err error, source string) {
		errorMsg := err.Error()
		fmt.Printf("Error en %s: %v\n", source, err)
		c.JSON(statusCode, gin.H{"error": errorMsg})
	}

	// Función para procesar la conversión y responder al cliente
	processConversion := func(inputData []byte, inputFormat string, source string) {
		fmt.Printf("Procesando video %s desde %s (%d bytes)\n", inputFormat, source, len(inputData))

		// Implementar recuperación de pánico
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("Recuperado de pánico en conversión: %v\n", r)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": fmt.Sprintf("Error interno durante la conversión: %v", r),
				})
			}
		}()

		convertedData, err := convertVideoToMp4(inputData, inputFormat)
		if err != nil {
			handleError(http.StatusInternalServerError, err, "conversión")
			return
		}

		// Verificar que los datos convertidos no estén vacíos
		if len(convertedData) == 0 {
			handleError(http.StatusInternalServerError,
				errors.New("la conversión produjo un archivo vacío"), "validación de salida")
			return
		}

		fmt.Printf("Conversión exitosa. Enviando respuesta (%d bytes)\n", len(convertedData))
		c.JSON(http.StatusOK, gin.H{
			"video": base64.StdEncoding.EncodeToString(convertedData),
			"format": "mp4",
		})
	}

	// Validar API Key
	if !validateAPIKey(c) {
		return
	}

	// Log para depuración
	fmt.Printf("Recibida solicitud de conversión de video a MP4. Content-Type: %s\n", c.ContentType())

	// Obtener formato de entrada
	inputFormat := c.DefaultPostForm("input_format", "mp4")

	// Verificar si hay una URL en el formulario
	formUrl := c.PostForm("url")
	if formUrl != "" {
		fmt.Printf("URL encontrada en form-data: %s\n", formUrl)
		inputData, err := fetchAudioFromURL(formUrl) // Reutilizamos la función existente
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de video (form)")
			return
		}
		processConversion(inputData, inputFormat, "form-data")
		return
	}

	// Verificar si hay una URL en los parámetros de consulta
	queryUrl := c.Query("url")
	if queryUrl != "" {
		fmt.Printf("URL encontrada en query params: %s\n", queryUrl)
		inputData, err := fetchAudioFromURL(queryUrl)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de video (query)")
			return
		}
		processConversion(inputData, inputFormat, "query params")
		return
	}

	// Verificar si hay datos en JSON
	var jsonData struct {
		URL         string `json:"url"`
		InputFormat string `json:"input_format"`
	}
	if err := c.ShouldBindJSON(&jsonData); err == nil && jsonData.URL != "" {
		fmt.Printf("URL encontrada en JSON: %s\n", jsonData.URL)
		inputData, err := fetchAudioFromURL(jsonData.URL)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de video (json)")
			return
		}

		// Usar el formato de entrada del JSON si está disponible
		if jsonData.InputFormat != "" {
			inputFormat = jsonData.InputFormat
		}

		processConversion(inputData, inputFormat, "JSON")
		return
	}

	// Si no hay URL, intentar otros métodos de entrada
	fmt.Println("No se encontró URL, intentando otros métodos de entrada")
	inputData, err := getInputData(c)
	if err != nil {
		handleError(http.StatusBadRequest, err, "obtención de datos de entrada")
		return
	}
	processConversion(inputData, inputFormat, "otros métodos")
}

func convertImageToPng(inputData []byte) ([]byte, error) {
	fmt.Printf("Iniciando conversión de imagen a PNG (%d bytes)\n", len(inputData))

	// Siempre usar archivos temporales para la conversión de imágenes
	return convertImageToPngUsingTempFiles(inputData)
}

// Función para convertir imagen a PNG usando archivos temporales
func convertImageToPngUsingTempFiles(inputData []byte) ([]byte, error) {
	fmt.Println("Usando archivos temporales para la conversión de imagen a PNG")

	// Crear archivo temporal para entrada sin extensión específica
	// para que FFmpeg auto-detecte el formato
	inputFile, err := os.CreateTemp("", "input-*")
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de entrada: %v", err)
	}
	inputPath := inputFile.Name()
	defer func() {
		inputFile.Close()
		os.Remove(inputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de entrada eliminado: %s\n", inputPath)
	}()

	// Escribir datos de entrada al archivo temporal
	bytesWritten, err := inputFile.Write(inputData)
	if err != nil {
		return nil, fmt.Errorf("error al escribir en archivo temporal: %v", err)
	}
	fmt.Printf("Datos escritos en archivo temporal: %d bytes en %s\n", bytesWritten, inputPath)
	inputFile.Close() // Cerrar archivo después de escribir

	// Crear archivo temporal para salida PNG
	outputFile, err := os.CreateTemp("", "output-*.png")
	if err != nil {
		return nil, fmt.Errorf("error al crear archivo temporal de salida: %v", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close() // Cerrar para que ffmpeg pueda escribir en él
	defer func() {
		os.Remove(outputPath) // Limpiar al finalizar
		fmt.Printf("Archivo temporal de salida eliminado: %s\n", outputPath)
	}()

	// Verificar que el archivo de entrada existe y tiene tamaño
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de entrada: %v", err)
	}
	fmt.Printf("Archivo de entrada verificado: %s (tamaño: %d bytes)\n", inputPath, inputInfo.Size())

	// Configurar comando ffmpeg para convertir a PNG
	cmd := exec.Command("ffmpeg",
		"-i", inputPath,          // Archivo de entrada
		"-f", "image2",           // Formato de imagen
		"-c:v", "png",            // Codec PNG
		"-y",                     // Sobrescribir sin preguntar
		outputPath)               // Archivo de salida

	// Capturar salida de error
	var errBuffer bytes.Buffer
	cmd.Stderr = &errBuffer

	fmt.Println("Ejecutando FFmpeg para conversión de imagen a PNG...")
	fmt.Printf("Comando: %v\n", cmd.Args)

	err = cmd.Run()
	if err != nil {
		fmt.Printf("Error durante la conversión de imagen: %v\n", err)
		fmt.Printf("Detalles del error: %s\n", errBuffer.String())
		return nil, fmt.Errorf("error en conversión de imagen: %v, detalles: %s", err, errBuffer.String())
	}

	// Verificar que el archivo de salida existe y tiene tamaño
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al verificar archivo de salida: %v", err)
	}
	fmt.Printf("Archivo de salida verificado: %s (tamaño: %d bytes)\n", outputPath, outputInfo.Size())

	// Leer archivo de salida
	outputData, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("error al leer archivo de salida: %v", err)
	}

	if len(outputData) == 0 {
		return nil, errors.New("la conversión produjo un archivo de salida vacío")
	}

	fmt.Printf("Conversión de imagen a PNG exitosa. Tamaño: %d bytes\n", len(outputData))
	return outputData, nil
}

func fetchImageFromURL(url string) ([]byte, error) {
	if url == "" {
		return nil, errors.New("URL vacía proporcionada")
	}

	fmt.Printf("Intentando descargar imagen desde: %s\n", url)

	// Configurar un cliente HTTP con timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error al crear solicitud: %v", err)
	}

	// Agregar User-Agent para evitar restricciones
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error al acceder URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("estado de respuesta inválido: %d", resp.StatusCode)
	}

	fmt.Printf("Descarga iniciada. Content-Length: %s\n", resp.Header.Get("Content-Length"))

	// Leer con un buffer para evitar problemas de memoria
	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error al leer datos: %v", err)
	}

	data := buffer.Bytes()
	fmt.Printf("Descarga completada. Tamaño: %d bytes\n", len(data))

	return data, nil
}

func processImageToPng(c *gin.Context) {
	// Función para manejar errores y responder al cliente
	handleError := func(statusCode int, err error, source string) {
		errorMsg := err.Error()
		fmt.Printf("Error en %s: %v\n", source, err)
		c.JSON(statusCode, gin.H{"error": errorMsg})
	}

	// Función para procesar la conversión y responder al cliente
	processConversion := func(inputData []byte, source string) {
		fmt.Printf("Procesando imagen a PNG desde %s (%d bytes)\n", source, len(inputData))

		// Implementar recuperación de pánico
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("Recuperado de pánico en conversión: %v\n", r)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": fmt.Sprintf("Error interno durante la conversión: %v", r),
				})
			}
		}()

		convertedData, err := convertImageToPng(inputData)
		if err != nil {
			handleError(http.StatusInternalServerError, err, "conversión")
			return
		}

		// Verificar que los datos convertidos no estén vacíos
		if len(convertedData) == 0 {
			handleError(http.StatusInternalServerError,
				errors.New("la conversión produjo un archivo vacío"), "validación de salida")
			return
		}

		fmt.Printf("Conversión exitosa. Enviando respuesta (%d bytes)\n", len(convertedData))
		c.JSON(http.StatusOK, gin.H{
			"image":  base64.StdEncoding.EncodeToString(convertedData),
			"format": "png",
		})
	}

	// Validar API Key
	if !validateAPIKey(c) {
		return
	}

	// Log para depuración
	fmt.Printf("Recibida solicitud de conversión de imagen a PNG. Content-Type: %s\n", c.ContentType())

	// Verificar si hay una URL en el formulario
	formUrl := c.PostForm("url")
	if formUrl != "" {
		fmt.Printf("URL encontrada en form-data: %s\n", formUrl)
		inputData, err := fetchImageFromURL(formUrl)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de imagen (form)")
			return
		}
		processConversion(inputData, "form-data")
		return
	}

	// Verificar si hay una URL en los parámetros de consulta
	queryUrl := c.Query("url")
	if queryUrl != "" {
		fmt.Printf("URL encontrada en query params: %s\n", queryUrl)
		inputData, err := fetchImageFromURL(queryUrl)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de imagen (query)")
			return
		}
		processConversion(inputData, "query params")
		return
	}

	// Verificar si hay datos en JSON
	var jsonData struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&jsonData); err == nil && jsonData.URL != "" {
		fmt.Printf("URL encontrada en JSON: %s\n", jsonData.URL)
		inputData, err := fetchImageFromURL(jsonData.URL)
		if err != nil {
			handleError(http.StatusBadRequest, err, "obtención de imagen (json)")
			return
		}
		processConversion(inputData, "JSON")
		return
	}

	// Si no hay URL, intentar otros métodos de entrada
	fmt.Println("No se encontró URL, intentando otros métodos de entrada")
	inputData, err := getInputData(c)
	if err != nil {
		handleError(http.StatusBadRequest, err, "obtención de datos de entrada")
		return
	}
	processConversion(inputData, "otros métodos")
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
	router.POST("/video-to-mp4", processVideoToMp4)
	router.POST("/convert-image-to-png", processImageToPng)

	router.Run(":" + port)
}
