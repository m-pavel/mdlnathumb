package main

import (
	"database/sql"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/giorgisio/goav/avcodec"
	"github.com/giorgisio/goav/avformat"
	"github.com/giorgisio/goav/avutil"
	"github.com/giorgisio/goav/swscale"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dir := flag.String("sdir", ".", "Directory to process")
	tdir := flag.String("tdir", "/var/db/minidlna/art_cache/", "Target directory")
	dbfile := flag.String("db", "/var/db/minidlna/files.db", "Minidlna db")
	force := flag.Bool("force", false, "Force recreate")
	flag.Parse()
	avformat.AvRegisterAll()
	rand.Seed(time.Now().UTC().UnixNano())

	log.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)

	db, err := sql.Open("sqlite3", *dbfile)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	filepath.Walk(*dir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		log.Printf("Processing %s\b", path)
		sqlStmt := fmt.Sprintf(`select RESOLUTION,THUMBNAIL,ALBUM_ART from DETAILS where PATH = '%s'`, path)
		rows, err := db.Query(sqlStmt)
		if err != nil {
			log.Println(err)
			return nil
		}
		var thumbnail bool
		var album_art int

		for rows.Next() {
			var resolution string

			err = rows.Scan(&resolution, &thumbnail, &album_art)
			if err != nil {
				log.Println(err)
				return nil
			}
			fmt.Println(resolution, thumbnail, album_art)
		}
		err = rows.Err()
		if err != nil {
			log.Println(err)
			return nil
		}
		if album_art != 0 && thumbnail && !*force {
			log.Printf("Already have thumbnail %s. Skipping.", path)
			return nil
		}
		if err != nil {
			log.Printf("%q: %s\n", err, sqlStmt)
			return nil
		}

		if image, err := procFile(path, *tdir); err != nil {
			log.Printf("%s : %v\n", path, err)
		} else {
			sqlStmt := fmt.Sprintf(`INSERT INTO ALBUM_ART (PATH) VALUES ('%s')`, *image)
			if res, err := db.Exec(sqlStmt); err != nil {
				log.Println(err)
				return nil
			} else {
				if id, err := res.LastInsertId(); err != nil {
					log.Println(err)
					return nil
				} else {
					sqlStmt = fmt.Sprintf(`UPDATE DETAILS set THUMBNAIL=true,ALBUM_ART=%d where PATH = '%s'`, id, path)
					if _, err := db.Exec(sqlStmt); err != nil {
						log.Println(err)
					}
				}
			}
		}
		return nil
	})
}

func procFile(path string, ddir string) (*string, error) {
	ctx := avformat.AvformatAllocContext()
	// ffmpeg -i input.mp4 -ss 00:00:01.000 -vframes 1 output.png
	if avformat.AvformatOpenInput(&ctx, path, nil, nil) != 0 {
		return nil, fmt.Errorf("Error: Couldn't open file %s", path)
	}
	defer ctx.AvformatCloseInput()
	if ctx.AvformatFindStreamInfo(nil) < 0 {
		return nil, fmt.Errorf("Error: Couldn't find stream information")
	}
	//ctx.AvDumpFormat(0, path, 0)
	var image *string
	var err error
	// Find the first video stream
	for i := 0; i < int(ctx.NbStreams()); i++ {
		switch ctx.Streams()[i].CodecParameters().AvCodecGetType() {
		case avformat.AVMEDIA_TYPE_VIDEO:

			// Get a pointer to the codec context for the video stream
			pCodecCtxOrig := ctx.Streams()[i].Codec()
			// Find the decoder for the video stream
			pCodec := avcodec.AvcodecFindDecoder(avcodec.CodecId(pCodecCtxOrig.GetCodecId()))
			if pCodec == nil {
				return nil, fmt.Errorf("Unsupported codec")
			}
			// Copy context
			pCodecCtx := pCodec.AvcodecAllocContext3()
			if pCodecCtx.AvcodecCopyContext((*avcodec.Context)(unsafe.Pointer(pCodecCtxOrig))) != 0 {
				return nil, fmt.Errorf("Couldn't copy codec context")
			}

			// Open codec
			if pCodecCtx.AvcodecOpen2(pCodec, nil) < 0 {
				return nil, fmt.Errorf("Could not open codec")
			}

			// Allocate video frame
			pFrame := avutil.AvFrameAlloc()

			// Allocate an AVFrame structure
			pFrameRGB := avutil.AvFrameAlloc()
			if pFrameRGB == nil {
				return nil, fmt.Errorf("Unable to allocate RGB Frame")
			}

			// Determine required buffer size and allocate buffer
			numBytes := uintptr(avcodec.AvpictureGetSize(avcodec.AV_PIX_FMT_RGB24, pCodecCtx.Width(),
				pCodecCtx.Height()))
			buffer := avutil.AvMalloc(numBytes)

			// Assign appropriate parts of buffer to image planes in pFrameRGB
			// Note that pFrameRGB is an AVFrame, but AVFrame is a superset
			// of AVPicture
			avp := (*avcodec.Picture)(unsafe.Pointer(pFrameRGB))
			avp.AvpictureFill((*uint8)(buffer), avcodec.AV_PIX_FMT_RGB24, pCodecCtx.Width(), pCodecCtx.Height())

			// initialize SWS context for software scaling
			swsCtx := swscale.SwsGetcontext(
				pCodecCtx.Width(),
				pCodecCtx.Height(),
				(swscale.PixelFormat)(pCodecCtx.PixFmt()),
				pCodecCtx.Width(),
				pCodecCtx.Height(),
				avcodec.AV_PIX_FMT_RGB24,
				avcodec.SWS_BILINEAR,
				nil,
				nil,
				nil,
			)

			// Read frames and save first five frames to disk
			dFrame := rand.Intn(200-50) + 50
			frameNumber := 1
			packet := avcodec.AvPacketAlloc()
		outer:
			for ctx.AvReadFrame(packet) >= 0 {
				// Is this a packet from the video stream?
				if packet.StreamIndex() == i {
					// Decode video frame
					response := pCodecCtx.AvcodecSendPacket(packet)
					if response < 0 {
						fmt.Printf("Error while sending a packet to the decoder: %s\n", avutil.ErrorFromCode(response))
					}
					for response >= 0 {
						response = pCodecCtx.AvcodecReceiveFrame((*avcodec.Frame)(unsafe.Pointer(pFrame)))
						if response == avutil.AvErrorEAGAIN || response == avutil.AvErrorEOF || response == -11 {
							break
						} else if response < 0 {
							log.Println(response)
							return nil, fmt.Errorf("Error while receiving a frame from the decoder: %s\n", avutil.ErrorFromCode(response))
						}
						if frameNumber == dFrame {
							// Convert the image from its native format to RGB
							swscale.SwsScale2(swsCtx, avutil.Data(pFrame),
								avutil.Linesize(pFrame), 0, pCodecCtx.Height(),
								avutil.Data(pFrameRGB), avutil.Linesize(pFrameRGB))

							// Save the frame to disk
							log.Printf("Writing frame %d\n", frameNumber)
							if image, err = SaveFrame(ddir, path, pFrameRGB, pCodecCtx.Width(), pCodecCtx.Height(), frameNumber); err != nil {
								log.Println(err)
							}
							break outer
						}
						frameNumber++
					}
				}

				// Free the packet that was allocated by av_read_frame
				packet.AvFreePacket()
			}

			// Free the RGB image
			avutil.AvFree(buffer)
			avutil.AvFrameFree(pFrameRGB)

			// Free the YUV frame
			avutil.AvFrameFree(pFrame)

			// Close the codecs
			pCodecCtx.AvcodecClose()
			(*avcodec.Context)(unsafe.Pointer(pCodecCtxOrig)).AvcodecClose()

			// Stop after saving frames of first video straem
			break

		default:
		}
	}
	return image, nil
}
func SaveFrame(ddir string, media string, frame *avutil.Frame, width, height, frameNumber int) (*string, error) {
	// Open file

	fileName := path.Join(ddir, media[:len(media)-len(filepath.Ext(media))]+".jpg")
	if err := os.MkdirAll(filepath.Dir(fileName), os.ModePerm); err != nil {
		return nil, err
	}

	img := image.NewRGBA64(image.Rectangle{Min: image.Point{0, 0}, Max: image.Point{width, height}})
	for y := 0; y < height; y++ {
		data0 := avutil.Data(frame)[0]
		startPos := uintptr(unsafe.Pointer(data0)) + uintptr(y)*uintptr(avutil.Linesize(frame)[0])
		for i := 0; i < width*3; i += 3 {
			r := *(*uint8)(unsafe.Pointer(startPos + uintptr(i)))
			g := *(*uint8)(unsafe.Pointer(startPos + uintptr(i+1)))
			b := *(*uint8)(unsafe.Pointer(startPos + uintptr(i+2)))
			img.Set(i/3, y, color.RGBA{R: r, G: g, B: b, A: 1})
		}
	}

	file, err := os.Create(fileName)
	if err != nil {
		return nil, err
	}
	log.Printf("Saving to %s\n", fileName)
	defer file.Close()

	if err := jpeg.Encode(file, img, nil); err != nil {
		return nil, err
	}
	return &fileName, nil
}
