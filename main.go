// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log"
	"time"
	"errors"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	janus "github.com/notedit/janus-go"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func watchHandle(handle *janus.Handle) {
	// wait for event
	for {
		msg := <-handle.Events
		switch msg := msg.(type) {
		case *janus.SlowLinkMsg:
			log.Println("SlowLinkMsg type ", handle.ID)
		case *janus.MediaMsg:
			log.Println("MediaEvent type", msg.Type, "receiving", msg.Receiving)
		case *janus.WebRTCUpMsg:
			log.Println("WebRTCUp type ", handle.ID)
		case *janus.HangupMsg:
			log.Println("HangupEvent type ", handle.ID)
		case *janus.EventMsg:
			log.Printf("EventMsg %+v", msg.Plugindata.Data)
		}
	}
}

func subscriber(session *janus.Session, room int64, feeds []float64) {
	peerConnection := createNewPeerConnection()
	// We must offer to send media for Janus to send anything
	if _, err := peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		panic(err)
	}
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		fmt.Printf("Got new track\n")
	})


	fmt.Printf("feeds: %+v\n", feeds)
	if len(feeds) == 0 {
		return
	}

	var streams []map[string]interface{}
	for _, feed := range feeds {
		streams = append(streams, map[string]interface{}{
			"feed": feed,
		})
	}
	handle, err := session.Attach("janus.plugin.videoroom")
	if err != nil {
		panic(err)
	}
	go watchHandle(handle)
	msg, err := handle.Message(map[string]interface{}{
		"request": "join",
		"ptype":   "subscriber",
		"room":    room,
		"streams": streams,
	}, nil)
	if err != nil {
		panic(err)
	}

	sdpVal, ok := msg.Jsep["sdp"].(string)
	if !ok {
		panic("failed to cast")
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP: sdpVal,
	}

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	answer, answerErr := peerConnection.CreateAnswer(nil)
	if answerErr != nil {
		panic(answerErr)
	}

	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherComplete

	msg, err = handle.Message(map[string]interface{}{
		"request": "start",
	}, map[string]interface{}{
		"type": "answer",
		"sdp": peerConnection.LocalDescription().SDP,
		"trickle": false,
	})

	fmt.Printf("start msg: %+v\n", msg)
	if err != nil {
		panic(err)
	}

	select {}
}

func publishersToFeeds(publishers interface{}) ([]float64, error){
	casted, ok := publishers.([]interface{})
	if !ok {
		return nil, errors.New("publishers not array")
	}

	var feeds []float64
	for _, item := range casted {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.New("publisher not map")
		}
		id, ok := m["id"].(float64)
		if !ok {
			return nil, errors.New("id not int")
		}
		feeds = append(feeds, id)
	}
	return feeds, nil
}

func createNewPeerConnection() *webrtc.PeerConnection {
	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	return peerConnection
}

func main() {
	gst.Init(nil)
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.
	peerConnection := createNewPeerConnection()

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
	})
	// Create a audio track
	opusTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTrack(opusTrack); err != nil {
		panic(err)
	}

	// Create a video track
	vp8Track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/vp8"}, "video", "pion")
	if err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTrack(vp8Track); err != nil {
		panic(err)
	}

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	gateway, err := janus.Connect("ws://192.168.1.13:8188/janus")
	if err != nil {
		panic(err)
	}

	session, err := gateway.Create()
	if err != nil {
		panic(err)
	}

	handle, err := session.Attach("janus.plugin.videoroom")
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			if _, keepAliveErr := session.KeepAlive(); keepAliveErr != nil {
				panic(keepAliveErr)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	go watchHandle(handle)

	now := time.Now()
	userId := now.UnixMilli() % (1 << 31) 
	fmt.Printf("UserId: %+v\n", userId)
	const roomId = 1234

	joinMsg, err := handle.Message(map[string]interface{}{
		"request": "join",
		"ptype":   "publisher",
		"room":    roomId,
		"id":      userId,
	}, nil)
	if err != nil {
		panic(err)
	}

	publishers := joinMsg.Plugindata.Data["publishers"]
	feeds, err := publishersToFeeds(publishers)
	if err != nil {
		panic(err)
	}
	go subscriber(session, roomId, feeds)

	msg, err := handle.Message(map[string]interface{}{
		"request": "publish",
		"audio":   true,
		"video":   true,
		"data":    false,
	}, map[string]interface{}{
		"type":    "offer",
		"sdp":     peerConnection.LocalDescription().SDP,
		"trickle": false,
	})
	if err != nil {
		panic(err)
	}

	if msg.Jsep != nil {
		sdpVal, ok := msg.Jsep["sdp"].(string)
		if !ok {
			panic("failed to cast")
		}
		err = peerConnection.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdpVal,
		})
		if err != nil {
			panic(err)
		}

		// Start pushing buffers on these tracks
		pipelineForCodec("opus", []*webrtc.TrackLocalStaticSample{opusTrack}, "audiotestsrc")
		// pipelineForCodec("vp8", []*webrtc.TrackLocalStaticSample{vp8Track}, "videotestsrc")
		// pipelineAudio(opusTrack, "room.ogg")
		pipelineVideo(vp8Track, "room.mkv")
	}

	select {}
}

func playPipeline(track *webrtc.TrackLocalStaticSample, pipeline *gst.Pipeline) {
	err := pipeline.SetState(gst.StatePlaying)
	if err != nil {
		panic(err)
	}

	appSink, err := pipeline.GetElementByName("appsink")
	if err != nil {
		panic(err)
	}

	app.SinkFromElement(appSink).SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}

			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}

			samples := buffer.Map(gst.MapRead).Bytes()
			defer buffer.Unmap()

			if err := track.WriteSample(media.Sample{Data: samples, Duration: *buffer.Duration().AsDuration()}); err != nil {
				panic(err) //nolint
			}

			return gst.FlowOK
		},
	})

}

func pipelineVideo(track *webrtc.TrackLocalStaticSample, filename string) {
	pipelineStr := fmt.Sprintf("filesrc location=%s ! matroskademux ! appsink name=appsink", filename)

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		panic(err)
	}

	playPipeline(track, pipeline)
}

func pipelineAudio(track *webrtc.TrackLocalStaticSample, filename string) {
	pipelineStr := fmt.Sprintf("filesrc location=%s ! oggdemux ! appsink name=appsink", filename)

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		panic(err)
	}

	playPipeline(track, pipeline)
}

// Create the appropriate GStreamer pipeline depending on what codec we are working with
func pipelineForCodec(codecName string, tracks []*webrtc.TrackLocalStaticSample, pipelineSrc string) {
	pipelineStr := "appsink name=appsink"
	switch codecName {
	case "vp8":
		pipelineStr = pipelineSrc + " ! vp8enc error-resilient=partitions keyframe-max-dist=10 auto-alt-ref=true cpu-used=5 deadline=1 ! " + pipelineStr
	case "vp9":
		pipelineStr = pipelineSrc + " ! vp9enc ! " + pipelineStr
	case "h264":
		pipelineStr = pipelineSrc + " ! video/x-raw,format=I420 ! x264enc speed-preset=ultrafast tune=zerolatency key-int-max=20 ! video/x-h264,stream-format=byte-stream ! " + pipelineStr
	case "opus":
		pipelineStr = pipelineSrc + " ! opusenc ! " + pipelineStr
	case "pcmu":
		pipelineStr = pipelineSrc + " ! audio/x-raw, rate=8000 ! mulawenc ! " + pipelineStr
	case "pcma":
		pipelineStr = pipelineSrc + " ! audio/x-raw, rate=8000 ! alawenc ! " + pipelineStr
	default:
		panic("Unhandled codec " + codecName) //nolint
	}

	pipeline, err := gst.NewPipelineFromString(pipelineStr)
	if err != nil {
		panic(err)
	}

	if err = pipeline.SetState(gst.StatePlaying); err != nil {
		panic(err)
	}

	appSink, err := pipeline.GetElementByName("appsink")
	if err != nil {
		panic(err)
	}

	app.SinkFromElement(appSink).SetCallbacks(&app.SinkCallbacks{
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {
			sample := sink.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}

			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}

			samples := buffer.Map(gst.MapRead).Bytes()
			defer buffer.Unmap()

			for _, t := range tracks {
				if err := t.WriteSample(media.Sample{Data: samples, Duration: *buffer.Duration().AsDuration()}); err != nil {
					panic(err) //nolint
				}
			}

			return gst.FlowOK
		},
	})
}
