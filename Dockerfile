# --- build ---
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /unifi-ups-exporter .

# --- runtime ---
FROM scratch
COPY --from=build /unifi-ups-exporter /unifi-ups-exporter
EXPOSE 9199
ENTRYPOINT ["/unifi-ups-exporter"]
