# Evolution Audio Converter

This project is a microservice in Go that processes audio files, converts them to **opus** or **mp3** format, and returns both the duration of the audio and the converted file in base64. The service accepts audio files sent as **form-data**, **base64**, or **URL**.

## Requirements

Before starting, you'll need to have the following installed:

- [Go](https://golang.org/doc/install) (version 1.21 or higher)
- [Docker](https://docs.docker.com/get-docker/) (to run the project in a container)
- [FFmpeg](https://ffmpeg.org/download.html) (for audio processing).

## Installation

### Clone the Repository

Clone this repository to your local machine:

```bash
git clone https://github.com/EvolutionAPI/evolution-audio-converter.git
cd evolution-audio-converter
```

### Install Dependencies

Install the project dependencies:

```bash
go mod tidy
```

### Install FFmpeg

The service depends on **FFmpeg** to convert the audio. Make sure FFmpeg is installed on your system.

- On Ubuntu:

  ```bash
  sudo apt update
  sudo apt install ffmpeg
  ```

- On macOS (via Homebrew):

  ```bash
  brew install ffmpeg
  ```

- On Windows, download FFmpeg [here](https://ffmpeg.org/download.html) and add it to your system `PATH`.

### Configuration

Create a `.env` file in the project's root directory with the following configuration:

```env
PORT=4040
API_KEY=your_secret_api_key_here
```

This defines the port where the service will run.

## Running the Project

### Locally

To run the service locally, use the following command:

```bash
go run main.go -dev
```

The server will be available at `http://localhost:4040`.

### Using Docker

If you prefer to run the service in a Docker container, follow the steps below:

1. **Build the Docker image**:

   ```bash
   docker build -t audio-service .
   ```

2. **Run the container**:

   ```bash
   docker run -p 4040:4040 --env-file=.env audio-service
   ```

   This will start the container on the port specified in the `.env` file.

## How to Use

You can send `POST` requests to the `/process-audio` endpoint with an audio file in the following formats:

- **Form-data** (to upload files)
- **Base64** (to send the audio encoded in base64)
- **URL** (to send the link to the audio file)

### Authentication

All requests must include the `apikey` header with the value of the `API_KEY` configured in the `.env` file.

### Optional Parameters

- **`format`**: You can specify the format for conversion by passing the `format` parameter in the request. Supported values:
  - `mp3`
  - `ogg` (default)

### Example Requests Using cURL

#### Sending as Form-data

```bash
curl -X POST -F "file=@path/to/audio.mp3" http://localhost:4040/process-audio \
  -F "format=ogg" \
  -H "apikey: your_secret_api_key_here"
```

#### Sending as Base64

```bash
curl -X POST -d "base64=$(base64 path/to/audio.mp3)" http://localhost:4040/process-audio \
  -d "format=ogg" \
  -H "apikey: your_secret_api_key_here"
```

#### Sending as URL

```bash
curl -X POST -d "url=https://example.com/path/to/audio.mp3" http://localhost:4040/process-audio \
  -d "format=ogg" \
  -H "apikey: your_secret_api_key_here"
```

### Response

The response will be a JSON object containing the audio duration and the converted audio file in base64:

```json
{
  "duration": 120,
  "audio": "UklGR... (base64 of the file)",
  "format": "ogg"
}
```

- `duration`: The audio duration in seconds.
- `audio`: The converted audio file encoded in base64.
- `format`: The format of the converted file (`mp3` or `ogg`).

## License

This project is licensed under the [MIT](LICENSE) license.
