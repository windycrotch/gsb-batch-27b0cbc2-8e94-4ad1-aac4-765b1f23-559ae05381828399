package manifestgenerator

import (
	"fmt"
	"path"

	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/hls"
	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/mediachunk"
	"github.com/jordicenzano/go-ts-segmenter/manifestgenerator/tspacket"
	"github.com/jordicenzano/go-ts-segmenter/uploaders"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/httpuploader"
	"github.com/jordicenzano/go-ts-segmenter/uploaders/s3uploader"
	"github.com/sirupsen/logrus"
)

// Version Indicates the package version
var Version = "1.1.0"

// HlsDefaultVersion to use
const HlsDefaultVersion int = 3

// ChunkInitTypes types indicates where to put the init data (PAT and PMT)
type ChunkInitTypes int

const (
	// ChunkNoIni Necessary if you choose manual PIDs selection
	ChunkNoIni ChunkInitTypes = iota

	//ChunkInit Creates the init segment
	ChunkInit

	//ChunkInitStart Adds PAT and PAT at each chunk start (CC will be broken)
	ChunkInitStart
)

const (
	//GhostPrefixDefault ghost chunk prefix
	GhostPrefixDefault = ".growing_"

	//ChunkFileNumberLength chunk filenumber length
	ChunkFileNumberLength = 5

	//ChunkFileExtensionDefault default chunk extension
	ChunkFileExtensionDefault = ".ts"

	//ChunkInitFileName Init chunk filename
	ChunkInitFileName = "init"
)

const (
	// ChunkLengthToleranceS Tolerance calculating chunk length
	ChunkLengthToleranceS = 0.25
)

// packetTableTypes
type packetTableTypes int

const (
	// PatTable PAT
	PatTable = iota

	// PmtTable PAT
	PmtTable
)

// initState
type initStates int

const (
	// InitNotIni no PAT / PMT saved
	InitNotIni = iota

	// InitsavedPAT PAT saved, needs PMT too
	InitsavedPAT

	// InitsavedPMT PMT and PAT saved
	InitsavedPMT
)

type options struct {
	log                *logrus.Logger
	chunkOutputType    mediachunk.OutputTypes
	manifestOutputType hls.OutputTypes
	baseOutPath        string
	chunkBaseFilename  string
	targetSegmentDurS  float64
	chunkInitType      ChunkInitTypes
	autoPIDs           bool
	videoPID           int
	audioPID           int
	manifestType       hls.ManifestTypes
	liveWindowSize     int
	lhlsAdvancedChunks int
	httpUploader       *httpuploader.HTTPUploader
	s3Uploader         *s3uploader.S3Uploader

	// uploaderForTest, when non-nil, overrides the destination uploader
	// selected from the HTTP/S3 pointers. Used by reliability tests that
	// target an in-memory origin; nil in production.
	uploaderForTest uploaders.Uploader
}

// effectiveUploader resolves the uploader that should receive chunks and
// manifests, honoring a test injection if present.
func (o *options) effectiveUploader() uploaders.Uploader {
	if o.uploaderForTest != nil {
		return o.uploaderForTest
	}
	return selectUploader(o.chunkOutputType, o.manifestOutputType, o.httpUploader, o.s3Uploader)
}

// injectUploader sets a test-only destination uploader.
func (o *options) injectUploader(u uploaders.Uploader) {
	o.uploaderForTest = u
}

// selectUploader returns the uploader that matches the requested
// destination type, or nil for the local-file / no-output modes.
func selectUploader(chunkOutputType mediachunk.OutputTypes, manifestOutputType hls.OutputTypes, httpUploader *httpuploader.HTTPUploader, s3Uploader *s3uploader.S3Uploader) uploaders.Uploader {
	if chunkOutputType == mediachunk.ChunkOutputModeHTTPChunkedTransfer ||
		chunkOutputType == mediachunk.ChunkOutputModeHTTPRegular ||
		manifestOutputType == hls.HlsOutputModeHTTP {
		if httpUploader != nil {
			return httpUploader
		}
	}
	if chunkOutputType == mediachunk.ChunkOutputModeS3 || manifestOutputType == hls.HlsOutputModeS3 {
		if s3Uploader != nil {
			return s3Uploader
		}
	}
	return nil
}

// ManifestGenerator Creates the manifest and chunks the media
type ManifestGenerator struct {
	options options

	// Internal parsing data
	isInSync        bool
	bytesToNextSync int
	detectedPMTID   int

	// Current TS packet data
	tsPacket tspacket.TsPacket

	// Time counters
	chunkStartTimeS float64
	lastPCRS        float64

	// Packet counter
	processedPackets uint64

	//currentChunks info (1 element array for HLS)
	currentChunks     []mediachunk.Chunk
	currentChunkIndex uint64

	//currentChunk info
	initChunk *mediachunk.Chunk
	initState initStates

	// Packets used to save PAT and PMT (We know we'll break TS CC). Only used in ChunkInitStart mode
	tsInitPATPacket tspacket.TsPacket
	tsInitPMTPacket tspacket.TsPacket

	//Hls generator
	hlsChunklist hls.Hls

	//initialChunkCreation Flag tha indicates the first chunk[s] has been created
	fistChunkCreated bool

	// fatalErr holds the first terminal upload/publish error. Once set,
	// AddData and Close keep returning it instead of silently producing a
	// manifest that references objects the origin does not have.
	fatalErr error
}

// New Creates a chunklistgenerator instance
func New(
	log *logrus.Logger,
	chunkOutputType mediachunk.OutputTypes,
	manifestOutputType hls.OutputTypes,
	baseOutPath string,
	chunkBaseFilename string,
	chunkListFilename string,
	targetSegmentDurS float64,
	chunkInitType ChunkInitTypes,
	autoPIDs bool,
	videoPID int,
	audioPID int,
	manifestType hls.ManifestTypes,
	liveWindowSize int,
	lhlsAdvancedChunks int,
	httpUploader *httpuploader.HTTPUploader,
	s3Uploader *s3uploader.S3Uploader,
) ManifestGenerator {
	if log == nil {
		log = logrus.New()
		log.SetLevel(logrus.DebugLevel)
	}

	chunklistFileName := path.Join(baseOutPath, chunkListFilename)
	uploader := selectUploader(chunkOutputType, manifestOutputType, httpUploader, s3Uploader)

	mg := ManifestGenerator{
		options: options{
			log:                log,
			chunkOutputType:    chunkOutputType,
			manifestOutputType: manifestOutputType,
			baseOutPath:        baseOutPath,
			chunkBaseFilename:  chunkBaseFilename,
			targetSegmentDurS:  targetSegmentDurS,
			chunkInitType:      chunkInitType,
			autoPIDs:           autoPIDs,
			videoPID:           videoPID,
			audioPID:           audioPID,
			manifestType:       manifestType,
			liveWindowSize:     liveWindowSize,
			lhlsAdvancedChunks: lhlsAdvancedChunks,
			httpUploader:       httpUploader,
			s3Uploader:         s3Uploader,
			uploaderForTest:    nil, // production never injects a test uploader
		},
		isInSync:          false,
		bytesToNextSync:   0,
		detectedPMTID:     -1,
		tsPacket:          tspacket.New(tspacket.TsDefaultPacketSize),
		chunkStartTimeS:   -1.0,
		lastPCRS:          -1.0,
		processedPackets:  0,
		currentChunks:     nil,
		currentChunkIndex: 0,
		initChunk:         nil,
		initState:         InitNotIni,
		tsInitPATPacket:   tspacket.New(tspacket.TsDefaultPacketSize),
		tsInitPMTPacket:   tspacket.New(tspacket.TsDefaultPacketSize),
		hlsChunklist: hls.New(
			log,
			manifestType,
			HlsDefaultVersion,
			true,
			targetSegmentDurS,
			liveWindowSize+lhlsAdvancedChunks,
			chunklistFileName,
			"",
			manifestOutputType,
			uploader,
		),
		fistChunkCreated: false,
		fatalErr:         nil,
	}

	return mg
}

func (mg *ManifestGenerator) resync(buf []byte) []byte {
	mg.isInSync = false

	start := 0
	for {
		if start < len(buf) {
			if buf[start] == 0x47 {
				mg.isInSync = true
				break
			} else {
				start++
			}
		} else {
			break
		}
	}

	return buf[start:]
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}

func (mg *ManifestGenerator) isSavingMediaPacket() bool {
	ret := false
	if !mg.options.autoPIDs {
		// Manual detection PIDs
		ret = true
	} else {
		if mg.options.chunkInitType == ChunkInit || mg.options.chunkInitType == ChunkInitStart {
			if mg.initState == InitsavedPMT {
				ret = true
			}
		} else if mg.options.chunkInitType == ChunkNoIni {
			ret = true
		}
	}

	return ret
}

func (mg *ManifestGenerator) saveInitPacket(tableType packetTableTypes) error {
	if mg.options.chunkInitType == ChunkInit {
		return mg.addPacketToInitChunk(tableType)
	} else if mg.options.chunkInitType == ChunkInitStart {
		if tableType == PatTable || tableType == PmtTable {
			mg.saveInitChunkPacket(tableType)
		}
	}

	return nil
}

// processPacket parses one TS packet. The boolean keeps the historical
// "packet was valid / resync needed" semantics; the error is non-nil when
// a chunk or manifest publication fails, in which case the stream is
// poisoned and the error must reach the caller.
func (mg *ManifestGenerator) processPacket(forceChunk bool) (bool, error) {
	if !mg.tsPacket.Parse(mg.detectedPMTID) {
		return false, nil
	}

	// Detect video & audio PIDs
	if mg.options.autoPIDs {
		pmtID := mg.tsPacket.GetPATdata()
		if pmtID >= 0 {
			mg.detectedPMTID = pmtID

			// Save PAT
			if err := mg.saveInitPacket(PatTable); err != nil {
				return false, err
			}

			mg.options.log.Debug("Detected PAT. PMT ID: ", pmtID)
		}

		valid, Videoh264, AudioADTS, Other := mg.tsPacket.GetPMTdata()
		if valid {
			if len(Videoh264) > 0 {
				mg.options.videoPID = int(Videoh264[0])
			}
			if len(AudioADTS) > 0 {
				mg.options.audioPID = int(AudioADTS[0])
			}

			// Save PMT
			if err := mg.saveInitPacket(PmtTable); err != nil {
				return false, err
			}

			mg.options.log.Debug("Detected PMT. VideoIDs: ", Videoh264, "AudiosIDs: ", AudioADTS, "Other: ", Other)
		}
	}

	pID := mg.tsPacket.GetPID()
	if pID == mg.options.videoPID {
		if mg.isSavingMediaPacket() {
			// Detect if we need to chunk it
			// It will chunk if detect an IDR point with PCR data
			if mg.tsPacket.IsRandomAccess(mg.options.videoPID) == true {
				mg.options.log.Debug("VIDEO: ", mg.tsPacket.String())
				pcrS := mg.tsPacket.GetPCRS()
				if pcrS >= 0 {
					mg.lastPCRS = pcrS

					if mg.chunkStartTimeS < 0 && pcrS >= 0 {
						mg.chunkStartTimeS = pcrS
					}
					durS := pcrS - mg.chunkStartTimeS
					if (durS + ChunkLengthToleranceS) > mg.options.targetSegmentDurS {
						nextChunkErr := error(nil)
						_, nextInitialPCRS, nextChunkErr := mg.nextChunk(pcrS, mg.chunkStartTimeS, tspacket.MaxPCRSValue, false)
						if nextChunkErr != nil {
							return false, nextChunkErr
						}

						mg.chunkStartTimeS = nextInitialPCRS
					}
				}
			}
			if err := mg.addPacketToChunk(); err != nil {
				return false, err
			}

		} else {
			mg.options.log.Debug("SKIPPED VIDEO PACKET, not init: ", mg.tsPacket.String())
		}
	} else if pID == mg.options.audioPID {
		if mg.isSavingMediaPacket() {
			if err := mg.addPacketToChunk(); err != nil {
				return false, err
			}
			mg.options.log.Debug("AUDIO: ", mg.tsPacket.String())
		} else {
			mg.options.log.Debug("SKIPPED AUDIO PACKET, not init: ", mg.tsPacket.String())
		}
	} else if pID >= 0 {
		mg.options.log.Debug("OTHER: ", mg.tsPacket.String())
	} else {
		fmt.Println("OUT OF SYNC!!!")
		return false, nil
	}

	return true, nil
}

func (mg *ManifestGenerator) addPacketToChunk() error {

	if mg.currentChunks == nil {
		if err := mg.createChunk(false); err != nil {
			return err
		}
	}

	if len(mg.currentChunks) > 0 {

		//In case we need to save PAT and PMT do it just before the 1st packet
		if mg.options.chunkInitType == ChunkInitStart && mg.currentChunks[0].IsEmpty() {
			// Save PAT and PMT first if available
			if mg.initState == InitsavedPMT {
				if err := mg.currentChunks[0].AddData(mg.tsInitPATPacket.GetBuffer()); err != nil {
					return err
				}
				if err := mg.currentChunks[0].AddData(mg.tsInitPMTPacket.GetBuffer()); err != nil {
					return err
				}
			}
		}

		if err := mg.currentChunks[0].AddData(mg.tsPacket.GetBuffer()); err != nil {
			return err
		}
	}

	return nil
}

func (mg *ManifestGenerator) saveInitChunkPacket(tableType packetTableTypes) bool {
	ret := false

	if tableType == PatTable {
		if mg.initState == InitNotIni {
			// Save PAT
			mg.tsInitPATPacket = tspacket.CloneFrom(mg.tsPacket)
			mg.initState = InitsavedPAT
			ret = true
		}
	} else if tableType == PmtTable {
		if mg.initState == InitsavedPAT {
			// Save PMT
			mg.tsInitPMTPacket = tspacket.CloneFrom(mg.tsPacket)
			mg.initState = InitsavedPMT
			ret = true
		}
	}

	return ret
}

func (mg *ManifestGenerator) addPacketToInitChunk(tableType packetTableTypes) error {
	saveData := false

	if tableType == PatTable {
		if mg.initState == InitNotIni { // We only save the 1st PAT PMT appeareance, so no dynamic updates are allowed
			if mg.initChunk == nil {
				// Create init chunk
				if err := mg.createChunk(true); err != nil {
					return err
				}
			}
			saveData = true
		}
	} else if tableType == PmtTable {
		if mg.initState == InitsavedPAT {
			saveData = true
		}
	}

	if saveData {
		if err := mg.initChunk.AddData(mg.tsPacket.GetBuffer()); err != nil {
			return err
		}

		if tableType == PatTable {
			mg.initState = InitsavedPAT
		} else if tableType == PmtTable {
			mg.initState = InitsavedPMT

			if err := mg.closeChunk(true, -1, false); err != nil {
				return err
			}
		}
	}

	return nil
}

func (mg *ManifestGenerator) hlsClose() error {
	return mg.hlsChunklist.CloseManifest(true)
}

// hlsAddChunk publishes the updated chunklist after a chunk became
// available. Any publication error is returned.
func (mg *ManifestGenerator) hlsAddChunk(isGrowing bool, fileName string, durationS float64, isDisco bool) error {
	return mg.hlsChunklist.AddChunk(hls.Chunk{IsGrowing: isGrowing, FileName: fileName, DurationS: durationS, IsDisco: isDisco}, true)
}

// closeChunk finalizes the current chunk.
//
// Ordering guarantee (non-LHLS): the chunk is fully uploaded FIRST and the
// chunklist is updated only when Close returned nil, i.e. the object is
// provably available at the destination. A failed upload aborts the flow
// and the chunk is never added to the manifest, so players cannot be
// pointed at a missing object.
func (mg *ManifestGenerator) closeChunk(isInit bool, chunkDurationS float64, isFinalChunk bool) error {
	// Close current

	if isInit == false {
		if mg.currentChunks != nil && len(mg.currentChunks) > 0 {
			currentChunk := mg.currentChunks[0]

			// 1) Make the object available (local write / external upload
			//    with bounded retries). This must succeed before any
			//    manifest can reference it.
			if err := currentChunk.Close(chunkDurationS); err != nil {
				return err
			}

			//NO LHLS
			if mg.options.lhlsAdvancedChunks <= 0 {
				// 2) Only now publish the chunklist referencing the chunk.
				if err := mg.hlsAddChunk(false, currentChunk.GetFilename(), chunkDurationS, false); err != nil {
					return err
				}
				if mg.options.manifestType == hls.Vod {
					if isFinalChunk {
						if err := mg.hlsClose(); err != nil {
							return err
						}
					}
				}
			}

			if len(mg.currentChunks) > 1 {
				// Remove 1st element
				mg.currentChunks = mg.currentChunks[1:]
			} else {
				// Empty array
				mg.currentChunks = mg.currentChunks[:0]
			}

			mg.currentChunkIndex++
		}
	} else {
		if mg.initChunk != nil {
			if err := mg.initChunk.Close(-1); err != nil {
				return err
			}

			mg.hlsChunklist.SetInitChunk(mg.initChunk.GetFilename())

			// We need to update version 7 for map chunks
			mg.hlsChunklist.SetHlsVersion(7)

			mg.initChunk = nil
		}
	}

	return nil
}

func (mg *ManifestGenerator) createChunk(isInit bool) error {
	// Close current
	if isInit {
		chunkInitOptions := mediachunk.Options{
			Log:                mg.options.log,
			OutputType:         mg.options.chunkOutputType,
			LHLS:               false,
			EstimatedDurationS: -1,
			FileNumberLength:   ChunkFileNumberLength,
			GhostPrefix:        GhostPrefixDefault,
			FileExtension:      ChunkFileExtensionDefault,
			BasePath:           mg.options.baseOutPath,
			ChunkBaseFilename:  ChunkInitFileName,
			Uploader:           mg.options.effectiveUploader(),
		}

		newChunk := mediachunk.New(0, chunkInitOptions)
		mg.initChunk = &newChunk

		if err := mg.initChunk.InitializeChunk(); err != nil {
			return err
		}
	} else {
		chunksToCreate := 1
		if mg.fistChunkCreated == false && mg.options.lhlsAdvancedChunks > 0 {
			chunksToCreate = mg.options.lhlsAdvancedChunks
			mg.fistChunkCreated = true
		}

		n := 0
		for n < chunksToCreate {
			chunkOptions := mediachunk.Options{
				Log:                mg.options.log,
				OutputType:         mg.options.chunkOutputType,
				LHLS:               false,
				EstimatedDurationS: mg.options.targetSegmentDurS,
				FileNumberLength:   ChunkFileNumberLength,
				GhostPrefix:        GhostPrefixDefault,
				FileExtension:      ChunkFileExtensionDefault,
				BasePath:           mg.options.baseOutPath,
				ChunkBaseFilename:  mg.options.chunkBaseFilename,
				Uploader:           mg.options.effectiveUploader()}

			if mg.options.lhlsAdvancedChunks > 0 {
				chunkOptions.LHLS = true
			}

			newChunk := mediachunk.New(mg.currentChunkIndex+uint64(len(mg.currentChunks)), chunkOptions)

			if err := newChunk.InitializeChunk(); err != nil {
				return err
			}

			// Add the advanced chunk to the manifest with target dur.
			// LHLS pre-announces growing chunks by design; keep that
			// historical behavior but surface publication errors.
			if mg.options.lhlsAdvancedChunks > 0 {
				if err := mg.hlsAddChunk(true, newChunk.GetFilename(), mg.options.targetSegmentDurS, false); err != nil {
					return err
				}
			}

			mg.currentChunks = append(mg.currentChunks, newChunk)

			n++
		}
	}
	return nil
}

// Creates chunk and returns the initial time for the next chunk
func (mg *ManifestGenerator) nextChunk(currentPCRS float64, lastInitialPCRS float64, maxPCRs float64, isFinalChunk bool) (chunkDurationS float64, nextInitialPCRS float64, err error) {
	chunkDurationS = -1.0
	nextInitialPCRS = currentPCRS

	if currentPCRS >= lastInitialPCRS {
		chunkDurationS = currentPCRS - lastInitialPCRS
	} else {
		// Detected possible PCR roll over
		mg.options.log.Info("Possible PCR rollover! lastInitialPCRS:", lastInitialPCRS, ", currentPCRS: ", currentPCRS, ", maxPCRs: ", maxPCRs)
		chunkDurationS = maxPCRs - currentPCRS + lastInitialPCRS
	}

	mg.options.log.Info("CHUNK! At PCRs: ", currentPCRS, ". ChunkDurS: ", chunkDurationS)

	if err = mg.closeChunk(false, chunkDurationS, isFinalChunk); err != nil {
		return
	}
	if !isFinalChunk {
		err = mg.createChunk(false)
	}

	return
}

// Close Closes manifest processing saving the last data and the last chunk.
//
// The final chunk and the final (ENDLIST) chunklist go through exactly the
// same upload + bounded-retry flow as regular data, so a process exiting on
// EOF can neither leave a half-written manifest nor temp files behind.
// The returned error is non-nil if that final delivery failed.
func (mg *ManifestGenerator) Close() error {
	if mg.fatalErr != nil {
		return mg.fatalErr
	}

	_, _, err := mg.nextChunk(mg.lastPCRS, mg.chunkStartTimeS, tspacket.MaxPCRSValue, true)
	if err != nil {
		mg.fatalErr = err
	}
	return err
}

// AddData feeds encoded TS data into the segmenter. It returns the first
// terminal upload/publish error encountered; after that the same error is
// returned for every subsequent call instead of silently continuing.
func (mg *ManifestGenerator) AddData(buf []byte) error {
	if mg.fatalErr != nil {
		return mg.fatalErr
	}

	if !mg.isInSync {
		buf = mg.resync(buf)

		if len(buf) > 0 {
			mg.bytesToNextSync = tspacket.TsDefaultPacketSize
		}
	}

	if len(buf) > 0 {
		addedSize := min(len(buf), mg.bytesToNextSync)
		mg.tsPacket.AddData(buf[:addedSize])

		mg.bytesToNextSync = mg.bytesToNextSync - addedSize

		buf = buf[addedSize:]
	}

	if mg.bytesToNextSync <= 0 {
		// Process packet
		valid, err := mg.processPacket(false)
		if err != nil {
			mg.fatalErr = err
			return err
		}
		if !valid {
			mg.isInSync = false
		} else {
			mg.bytesToNextSync = tspacket.TsDefaultPacketSize
			mg.processedPackets++
			mg.tsPacket.Reset()
		}
	}

	if len(buf) > 0 {
		// Still data to process
		if err := mg.AddData(buf[:]); err != nil {
			return err
		}
	}

	return nil
}

// FatalErr returns the first terminal delivery error that poisoned the
// stream, or nil while every chunk and manifest publication succeeded.
func (mg *ManifestGenerator) FatalErr() error {
	return mg.fatalErr
}

func (mg ManifestGenerator) getNumProcessedPackets() uint64 {
	return mg.processedPackets
}
