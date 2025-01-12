#!/bin/bash
docker buildx build --platform linux/amd64,linux/arm64 -t atendai/evolution-audio-converter:latest --push .
