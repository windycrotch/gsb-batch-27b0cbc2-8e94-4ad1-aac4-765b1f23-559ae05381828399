package hls

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/sirupsen/logrus"
)

// Version Indicates the package version
var Version = "1.0.0"

// ManifestTypes indicates the manifest type
type ManifestTypes int

const (
	// Vod Indicates VOD manifest
	Vod ManifestTypes = iota

	//LiveEvent Indicates a live manifest type event (always growing)
	LiveEvent

	//LiveWindow Indicates a live manifest type sliding window (fixed size)
	LiveWindow
)

// OutputTypes indicates the manifest type
type OutputTypes int

const (
	// HlsOutputModeNone No no write data
	HlsOutputModeNone OutputTypes = iota

	// HlsOutputModeFile Saves data to file
	HlsOutputModeFile

	// HlsOutputModeHTTP data to HTTP streaming server
	HlsOutputModeHTTP

	// HlsOutputModeS3 data to S3 (using AWS API)
	HlsOutputModeS3
)

// Chunk Chunk information
type Chunk struct {
	IsGrowing bool
	FileName  string
	DurationS float64
	IsDisco   bool
}

// Hls Hls chunklist
type Hls struct {
	// mu guards every piece of state below. All public methods are safe
	// for concurrent use: a manifest must never be published referencing
	// a chunk while another goroutine is mutating the chunk list.
	// It is a pointer so Hls keeps its historical value semantics without
	// copying a lock (and stays go vet clean).
	mu                    *sync.Mutex
	log                   *logrus.Logger
	manifestType          ManifestTypes
	version               int
	isIndependentSegments bool
	targetDurS            float64
	slidingWindowSize     int
	mseq                  int64
	dseq                  int64
	chunks                []Chunk
	chunklistFileName     string
	initChunkDataFileName string
	outputType            OutputTypes
	uploader              uploaders.Uploader
	isClosed              bool

	// dirty is set when the latest in-memory state could not be fully
	// published (local write or external upload failure). It stays set
	// until the next successful save, which republishes the whole
	// chunklist and therefore restores a consistent state at the
	// destination without ever referencing an unavailable object.
	dirty bool
}

// New Creates a hls chunklist manifest
func New(
	log *logrus.Logger,
	ManifestType ManifestTypes,
	version int,
	isIndependentSegments bool,
	targetDurS float64,
	slidingWindowSize int,
	chunklistFileName string,
	initChunkDataFileName string,
	outputType OutputTypes,
	uploader uploaders.Uploader,
) Hls {
	if log == nil {
		log = logrus.New()
		log.SetLevel(logrus.DebugLevel)
	}

	h := Hls{
		mu:                    &sync.Mutex{},
		log:                   log,
		manifestType:          ManifestType,
		version:               version,
		isIndependentSegments: isIndependentSegments,
		targetDurS:            targetDurS,
		slidingWindowSize:     slidingWindowSize,
		mseq:                  0,
		dseq:                  0,
		chunks:                make([]Chunk, 0),
		chunklistFileName:     chunklistFileName,
		initChunkDataFileName: initChunkDataFileName,
		outputType:            outputType,
		uploader:              uploader,
		isClosed:              false,
	}

	return h
}

// SetInitChunk Adds a chunk init infomation
func (p *Hls) SetInitChunk(initChunkFileName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initChunkDataFileName = initChunkFileName
}

// IsDirty reports whether the current in-memory state has not been fully
// published yet. Mainly useful for tests and operational checks.
func (p *Hls) IsDirty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirty
}

// SetUploaderForTest replaces the destination uploader. It exists so
// reliability tests can point the publisher at an in-memory origin.
func (p *Hls) SetUploaderForTest(u uploaders.Uploader) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uploader = u
}

// saveChunklist publishes the current in-memory state. The caller must
// hold p.mu. A failed publication marks the chunklist dirty; the next
// successful call republishes the complete state (self-healing).
func (p *Hls) saveChunklist() error {
	hlsStrByte := []byte(p.renderLocked())

	var err error
	switch p.outputType {
	case HlsOutputModeFile:
		err = p.saveManifestToFile(hlsStrByte)
	case HlsOutputModeHTTP, HlsOutputModeS3:
		err = p.saveManifestExternal(hlsStrByte, p.outputType)
	case HlsOutputModeNone:
		err = nil
	default:
		err = nil
	}

	if err != nil {
		p.dirty = true
		return err
	}

	p.dirty = false
	return nil
}

// CloseManifest finalizes the manifest (adds ENDLIST on the next render)
// and forces one last publication. It returns an error when the final
// chunklist could not be delivered, so the Close path cannot finish
// silently leaving a half-written or stale manifest at the destination.
func (p *Hls) CloseManifest(saveChunklist bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.isClosed = true

	if saveChunklist {
		return p.saveChunklist()
	}

	return nil
}

// SetHlsVersion Sets manifest version
func (p *Hls) SetHlsVersion(version int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.version = version
}

// saveManifestToFile writes the manifest atomically: data goes to a temp
// file in the same directory and is renamed over the destination, so a
// reader can never observe a partially written chunklist.
func (p *Hls) saveManifestToFile(manifestByte []byte) error {
	if p.chunklistFileName == "" {
		return nil
	}

	dir := filepath.Dir(p.chunklistFileName)
	if dir != "" && dir != "." {
		if errMkdir := os.MkdirAll(dir, 0744); errMkdir != nil {
			return errMkdir
		}
	}

	tmpFile, errTmp := ioutil.TempFile(dir, ".chunklist-*.tmp")
	if errTmp != nil {
		return errTmp
	}
	tmpName := tmpFile.Name()

	if _, errWrite := tmpFile.Write(manifestByte); errWrite != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return errWrite
	}
	if errSync := tmpFile.Sync(); errSync != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return errSync
	}
	if errClose := tmpFile.Close(); errClose != nil {
		os.Remove(tmpName)
		return errClose
	}

	if errRename := os.Rename(tmpName, p.chunklistFileName); errRename != nil {
		os.Remove(tmpName)
		return errRename
	}

	return nil
}

func (p *Hls) saveManifestExternal(manifestByte []byte, outputType OutputTypes) error {
	if p.chunklistFileName == "" || p.uploader == nil {
		return nil
	}

	h := make(map[string]string)
	if strings.ToLower(path.Ext(p.chunklistFileName)) == ".m3u8" {
		h["Content-Type"] = "application/vnd.apple.mpegurl"
	}

	return p.uploader.UploadData(manifestByte, p.chunklistFileName, h)
}

// AddChunk registers a new chunk and publishes the resulting chunklist.
//
// The chunk is appended to the in-memory state BEFORE the publication so
// that any successful save already references it; conversely, when the
// publication fails the error is returned and the dirty flag is kept,
// meaning the caller (manifest generator) must not treat the chunk as
// available. The next successful save republishes the full list,
// restoring consistency at the destination.
func (p *Hls) AddChunk(chunkData Chunk, saveChunklist bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.chunks = append(p.chunks, chunkData)

	if p.manifestType == LiveWindow && len(p.chunks) > p.slidingWindowSize {
		//Remove first
		if p.chunks[0].IsDisco {

		}
		p.chunks = p.chunks[1:]
		p.mseq++
	}

	if saveChunklist {
		return p.saveChunklist()
	}

	return nil
}

// String returns the serialized manifest.
func (p *Hls) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.renderLocked()
}

// renderLocked serializes the current state. The caller must hold p.mu.
func (p *Hls) renderLocked() string {
	var buffer bytes.Buffer

	buffer.WriteString("#EXTM3U\n")
	buffer.WriteString("#EXT-X-VERSION:" + strconv.Itoa(p.version) + "\n")
	buffer.WriteString("#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(p.mseq, 10) + "\n")
	buffer.WriteString("#EXT-X-DISCONTINUITY-SEQUENCE:" + strconv.FormatInt(p.dseq, 10) + "\n")

	if p.manifestType == Vod {
		buffer.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	} else if p.manifestType == LiveEvent {
		buffer.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}

	buffer.WriteString("#EXT-X-TARGETDURATION:" + fmt.Sprintf("%.0f", p.targetDurS) + "\n")

	if p.isIndependentSegments {
		buffer.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}

	if p.initChunkDataFileName != "" {
		chunkPath := p.relativeURILocked(p.initChunkDataFileName)
		buffer.WriteString("#EXT-X-MAP:URI=\"" + chunkPath + "\"\n")
	}

	for _, chunk := range p.chunks {
		if chunk.IsDisco {
			buffer.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		buffer.WriteString("#EXTINF:" + fmt.Sprintf("%.8f", chunk.DurationS) + ",\n")

		buffer.WriteString(p.relativeURILocked(chunk.FileName) + "\n")
	}

	if p.isClosed {
		buffer.WriteString("#EXT-X-ENDLIST\n")
	}

	return buffer.String()
}

// relativeURILocked renders an object path as a URI relative to the
// chunklist directory. If a relative path cannot be computed (for example
// the two paths live on different Windows volumes), it falls back to the
// base file name instead of emitting an empty URI, which would produce a
// manifest pointing at nothing.
func (p *Hls) relativeURILocked(objectPath string) string {
	rel, err := filepath.Rel(path.Dir(p.chunklistFileName), objectPath)
	if err != nil || rel == "" {
		return filepath.Base(objectPath)
	}
	return rel
}
