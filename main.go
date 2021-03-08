package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/format/rtsp"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port        int `json:"port"`
	RtspTimeout int `json:"rtsp_timeout"`
	Rtp_timeout int `json:"rtp_timeout"`
}

type Session struct {
	Rtsp                string
	Track               *webrtc.TrackLocalStaticSample
	PeerConnectionCount int64
	Webrtc              string
}

var (
	sessions map[string]*Session
	config   Config
)

func doSignaling(c *gin.Context) {
	session := sessions[c.Param("uuid")]
	if session == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Session not found"})
		return
	}

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs:       []string{"turn:127.0.0.1:3478"},
				Credential: "megacam",
				Username:   "megacam",
			},
		},
	})
	if err != nil {
		panic(err)
	}

	if err != nil {
		panic(err)
	}

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		if connectionState == webrtc.ICEConnectionStateDisconnected {
			log.Println("Disconnect " + session.Rtsp)
			peerConnection.GetTransceivers()[0].Stop()
			peerConnection.GetTransceivers()[0].Receiver().Stop()
			peerConnection.GetTransceivers()[0].Sender().Stop()
			peerConnection.Close()
			peerConnection = nil
			return
		} else if connectionState == webrtc.ICEConnectionStateConnected {
			log.Println("Connect " + session.Rtsp)
		}
	})

	if _, err = peerConnection.AddTrack(session.Track); err != nil {
		panic(err)
	}

	var offer webrtc.SessionDescription
	if err = c.Bind(&offer); err != nil {
		panic(err)
	}

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherCompletePromise

	response, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	c.Header("Content-Type", "application/json")
	if _, err := c.Writer.Write(response); err != nil {
		panic(err)
	}
	return
}

func rtspToWebrtc(c *gin.Context) {
	var rtsp = struct {
		Rtsp string `json:"rtsp"`
	}{}

	if err := c.Bind(&rtsp); !errors.Is(err, nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err})
		return
	}

	for _, session := range sessions {
		if strings.EqualFold(session.Rtsp, rtsp.Rtsp) {
			c.JSON(http.StatusOK, gin.H{"webrtc": session.Webrtc})
			return
		}
	}

	var session Session
	var err error
	id := uuid.NewString()
	session.Track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: "video/h264",
	}, id, id)
	session.Rtsp = rtsp.Rtsp
	session.Webrtc = "http://" + c.Request.Host + "/webrtc/" + id
	if err != nil {
		panic(err)
	}

	go rtspConsumer(session, id)

	c.JSON(http.StatusOK, gin.H{"webrtc": session.Webrtc})
}

func main() {
	//gin.SetMode(gin.ReleaseMode)
	sessions = make(map[string]*Session)
	file, _ := os.Open("./config.json")
	decoder := json.NewDecoder(file)
	err := decoder.Decode(&config)
	if err != nil {
		log.Println(err)
	}
	r := gin.Default()
	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"*"},
		AllowHeaders: []string{"*"},
	}))
	r.POST("/webrtc/:uuid", doSignaling)
	r.POST("/rtsp-to-webrtc", rtspToWebrtc)
	r.Run(fmt.Sprintf(":%d", config.Port))
}

func rtspConsumer(sessionNow Session, id string) {
	annexbNALUStartCode := func() []byte { return []byte{0x00, 0x00, 0x00, 0x01} }
	sessions[id] = &sessionNow
	log.Println("Create session " + sessionNow.Rtsp)
	for {
		session, err := rtsp.Dial(sessionNow.Rtsp)
		if err != nil {
			log.Println(err)
			break
		}
		session.RtpKeepAliveTimeout = time.Duration(config.Rtp_timeout) * time.Second
		session.RtspTimeout = time.Duration(config.RtspTimeout) * time.Second
		codecs, err := session.Streams()
		if err != nil {
			log.Println(err)
			break
		}
		for i, t := range codecs {
			log.Println("Stream", i, "is of type", t.Type().String())
		}
		if codecs[0].Type() != av.H264 {
			panic("RTSP feed must begin with a H264 codec")
		}
		if len(codecs) != 1 {
			log.Println("Ignoring all but the first stream.")
		}

		var previousTime time.Duration
		for {
			pkt, err := session.ReadPacket()
			if err != nil {
				break
			}

			if pkt.Idx != 0 {
				continue
			}

			pkt.Data = pkt.Data[4:]

			if pkt.IsKeyFrame {
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
				pkt.Data = append(codecs[0].(h264parser.CodecData).PPS(), pkt.Data...)
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
				pkt.Data = append(codecs[0].(h264parser.CodecData).SPS(), pkt.Data...)
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
			}

			bufferDuration := pkt.Time - previousTime
			previousTime = pkt.Time
			if err = sessionNow.Track.WriteSample(media.Sample{Data: pkt.Data, Duration: bufferDuration}); err != nil && err != io.ErrClosedPipe {
				sessions[id] = nil
				log.Println(err)
				break
			}
		}

		if err = session.Close(); err != nil {
			log.Println("session Close error", err)
			break
		}

		time.Sleep(5 * time.Second)
	}
	sessions[id] = nil
}
