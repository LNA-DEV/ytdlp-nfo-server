FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o ytdlp-nfo-server .

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir yt-dlp git+https://github.com/LNA-DEV/ytdlp-nfo.git
COPY --from=builder /app/ytdlp-nfo-server /usr/local/bin/
EXPOSE 8080
ENV DOWNLOAD_DIR=/downloads
VOLUME /downloads
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["entrypoint.sh"]
