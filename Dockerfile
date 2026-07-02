FROM golang:1.25-bookworm AS build
WORKDIR /app

# go.sum намеренно не копируется: он генерируется командой go mod tidy ниже.
COPY go.mod ./

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=1 GOOS=linux go build -o /bot ./cmd/bot

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y \
    ca-certificates \
    tzdata \
    tesseract-ocr \
    tesseract-ocr-rus \
    tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app

COPY --from=build /bot /app/bot
COPY assets/fonts /app/assets/fonts

RUN mkdir -p /app/data /app/data/reports

ENV FONT_DIR=/app/assets/fonts
ENV SESSION_DB_PATH=/app/data/session.db
ENV REPORT_DIR=/app/data/reports

CMD ["/app/bot"]
