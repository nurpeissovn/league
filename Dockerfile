# --- build ---
    FROM golang:1.22-alpine AS build
    WORKDIR /app
    COPY go.mod go.sum ./
    RUN go mod download
    
    COPY . .
    # build the server from backend/main.go
    RUN CGO_ENABLED=0 GOOS=linux go build -o server ./backend/main.go
    
    # --- run ---
    FROM alpine:3.20
    WORKDIR /app
    COPY --from=build /app/server .
    COPY public ./public
    COPY init.sql ./init.sql
    
    ENV PORT=3000
    EXPOSE 3000
    CMD ["./server"]
    