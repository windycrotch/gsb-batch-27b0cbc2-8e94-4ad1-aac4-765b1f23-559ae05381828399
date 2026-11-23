package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	version   string
	buildTime string
	config    ConfigStruct
)

type IceServersStruct struct {
	IceServer string `yaml:"ice-server"`
}

type ConfigStruct struct {
	ServeIp          string             `yaml:"serve-ip"`
	ServePort        string             `yaml:"serve-port"`
	RtpHost          string             `yaml:"rtp-host"`
	EnableVideo      bool               `yaml:"enable-video"`
	EnableAudio      bool               `yaml:"enable-audio"`
	RtpVideoPort     int                `yaml:"rtp-video-port"`
	RtpAudioPort     int                `yaml:"rtp-audio-port"`
	VideoTrackName   string             `yaml:"video-track-name"`
	AudioTrackName   string             `yaml:"audio-track-name"`
	VideoCodec       string             `yaml:"video-codec"`
	AudioCodec       string             `yaml:"audio-codec"`
	IceServers       []IceServersStruct `yaml:"ice-servers"`
	ApiLocation      string             `yaml:"api-location"`
	AllowCrossOrigin bool               `yaml:"allow-cross-origin"`
}

func main() {
	log.Printf("Starting rtp2webrtc %s build on %s", version, buildTime)

	configFilePath := os.Getenv("RTP2WEBRTC_CONFIG_FILE_PATH")
	log.Printf("Loading configuration file located at %s", configFilePath)
	configFile, err := os.ReadFile(configFilePath)
	if err != nil {
		log.Fatal(err)
	}

	if err := yaml.Unmarshal(configFile, &config); err != nil {
		log.Fatal(err)
	}

	iceServersArray := make([]string, 0, len(config.IceServers))
	for _, iceServer := range config.IceServers {
		iceServersArray = append(iceServersArray, iceServer.IceServer)
	}

	media := make(map[string]*MediaSource)

	if config.EnableVideo {
		track, err := initWebrtcTrack("video", config.VideoCodec, config.VideoTrackName)
		if err != nil {
			log.Fatalf("cannot create video track: %v", err)
		}
		source := NewMediaSource("video", config.RtpHost, config.RtpVideoPort, track)
		media["video"] = source
		go source.Run()
	}

	if config.EnableAudio {
		track, err := initWebrtcTrack("audio", config.AudioCodec, config.AudioTrackName)
		if err != nil {
			log.Fatalf("cannot create audio track: %v", err)
		}
		source := NewMediaSource("audio", config.RtpHost, config.RtpAudioPort, track)
		media["audio"] = source
		go source.Run()
	}

	server := NewServer(config, iceServersArray, media)

	mux := http.NewServeMux()
	mux.HandleFunc(config.ApiLocation, server.ServeHTTP)

	httpServer := &http.Server{
		Addr:              config.ServeIp + ":" + config.ServePort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Stop media reception and release viewer sessions on SIGINT/SIGTERM.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Serving API on %s:%s", config.ServeIp, config.ServePort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	case sig := <-signalCh:
		log.Printf("received signal %v, shutting down", sig)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}

	server.Shutdown()
	for _, source := range media {
		source.Stop()
	}
	log.Printf("shutdown complete")
}
