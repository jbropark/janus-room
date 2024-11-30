// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	janus "github.com/notedit/janus-go"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

var wg sync.WaitGroup

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

func watchPubHandle(pubHandle *janus.Handle, peerConnection *webrtc.PeerConnection, handle *janus.Handle, room int64, feeds []float64) {
	// wait for event
	initialized := initSubscriber(peerConnection, handle, room, feeds)
	for {
		msg := <-pubHandle.Events
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
			feeds, err := publishersToFeeds(msg.Plugindata.Data["publishers"])
			if err != nil {
				panic(err)
			}
			if !initialized {
				initialized = initSubscriber(peerConnection, handle, room, feeds)
			} else {
				subscribeNew(peerConnection, handle, feeds)
			}
		}
	}
}

func subscribeNew(peerConnection *webrtc.PeerConnection, handle *janus.Handle, feeds []float64) {
	fmt.Printf("New subscribe")
	streams := feedsToStreams(feeds)
	msg, err := handle.Message(map[string]interface{}{
		"request": "subscribe",
		"streams": streams,
	}, nil)
	if err != nil {
		panic(err)
	}
	acceptOffer(peerConnection, handle, msg)
}

func feedsToStreams(feeds []float64) []map[string]interface{} {
	var streams []map[string]interface{}
	for _, feed := range feeds {
		streams = append(streams, map[string]interface{}{
			"feed": feed,
		})
	}
	return streams
}

func acceptOffer(peerConnection *webrtc.PeerConnection, handle *janus.Handle, msg *janus.EventMsg) {
	sdpVal, ok := msg.Jsep["sdp"].(string)
	if !ok {
		panic("failed to cast")
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpVal,
	}

	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	answer, answerErr := peerConnection.CreateAnswer(nil)
	if answerErr != nil {
		panic(answerErr)
	}

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherComplete

	_, err := handle.Message(map[string]interface{}{
		"request": "start",
	}, map[string]interface{}{
		"type":    "answer",
		"sdp":     peerConnection.LocalDescription().SDP,
		"trickle": false,
	})

	if err != nil {
		panic(err)
	}
}

func initSubscriber(peerConnection *webrtc.PeerConnection, handle *janus.Handle, room int64, feeds []float64) bool {
	if len(feeds) == 0 {
		return false
	}

	streams := feedsToStreams(feeds)
	msg, err := handle.Message(map[string]interface{}{
		"request": "join",
		"ptype":   "subscriber",
		"room":    room,
		"streams": streams,
	}, nil)
	if err != nil {
		panic(err)
	}
	acceptOffer(peerConnection, handle, msg)

	return true
}

func publishersToFeeds(publishers interface{}) ([]float64, error) {
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

var config = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	},
	SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
}

func readToDiscard(track *webrtc.TrackRemote) {
	for {
		_, _, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}
	}
}

func saveToDisk(ctx context.Context, writer media.Writer, track *webrtc.TrackRemote) {
	channel := make(chan struct {
		*rtp.Packet
		error
	})
	go func() {
		for {
			packet, _, err := track.ReadRTP()
			channel <- struct {
				*rtp.Packet
				error
			}{packet, err}
		}
	}()
	defer func() {
		if err := writer.Close(); err != nil {
			panic(err)
		}
		fmt.Printf("[%s] Finished saving to disk\n", track.ID())
		wg.Done()
	}()
	wg.Add(1)

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[%s] Stop saving to disk\n", track.ID())
			return
		case r := <-channel:
			if r.error != nil {
				panic(r.error)
			}

			if err := writer.WriteRTP(r.Packet); err != nil {
				panic(err)
			}
		}
	}
}

func saveOpusToDisk(ctx context.Context, track *webrtc.TrackRemote, path string) {
	codec := track.Codec()
	if len(path) == 0 {
		readToDiscard(track)
		return
	}

	writer, err := oggwriter.New(path, codec.ClockRate, codec.Channels)
	if err != nil {
		panic(err)
	}
	saveToDisk(ctx, writer, track)
}

func saveVP8ToDisk(ctx context.Context, track *webrtc.TrackRemote, path string) {
	if len(path) == 0 {
		readToDiscard(track)
		return
	}

	writer, err := ivfwriter.New(path)
	if err != nil {
		panic(err)
	}
	saveToDisk(ctx, writer, track)
}

var statsGetter stats.Getter

var BENCH_COLUMN_NAMES = []string{"track", "kind", "last_received_dt", "packet_received", "packet_lost", "jitter", "nack_count"}

var benchMutex sync.Mutex

func writeBench(writer *csv.Writer, trackID string, kind string, inbound stats.InboundRTPStreamStats) {
	benchMutex.Lock()
	defer benchMutex.Unlock()

	values := []string{
		trackID,
		kind,
		inbound.LastPacketReceivedTimestamp.Format("2006-01-02T15:04:05.000000"),
		strconv.FormatUint(inbound.PacketsReceived, 10),
		strconv.FormatInt(inbound.PacketsLost, 10),
		strconv.FormatFloat(inbound.Jitter, 'f', -1, 64),
		strconv.FormatUint(uint64(inbound.NACKCount), 10),
	}
	writer.Write(values)
}

func saveBench(ctx context.Context, track *webrtc.TrackRemote, writer *csv.Writer, interval int) {
	trackID := track.ID()

	defer func() {
		fmt.Printf("[%s] Finished writing bench\n", trackID)
		wg.Done()
	}()
	wg.Add(1)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	count := 0
	ssrc := uint32(track.SSRC())
	kind := track.Kind().String()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[%s] Stop saving bench\n", trackID)
			return
		case <-ticker.C:
			stats := statsGetter.Get(ssrc)
			inbound := stats.InboundRTPStreamStats

			writeBench(writer, trackID, kind, inbound)

			count += 1
			fmt.Printf("[%s] (%d / -) %v\n", trackID, count, inbound)

			/*
				inbound.PacketsReceived
				inbound.PacketsLost
				inbound.Jitter
				inbound.LastPacketReceivedTimestamp
				inbound.HeaderBytesReceived
				inbound.BytesReceived
				inbound.NACKCount
			*/
		}
	}
}

func createWebRTCAPI(engine *webrtc.MediaEngine, registry *interceptor.Registry) *webrtc.API {
	if err := engine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	statsInterceptorFactory.OnNewPeerConnection(func(_ string, getter stats.Getter) {
		statsGetter = getter
	})
	registry.Add(statsInterceptorFactory)

	if err = webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		panic(err)
	}

	return webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry))
}

func main() {
	argHost := flag.String("host", "192.168.1.13", "janus ip")
	argPort := flag.Int("port", 8188, "janus websocket port")
	argRoomID := flag.Int("room", 1234, "target room id")
	argBench := flag.Bool("bench", false, "enable bench")
	argAudio := flag.Bool("audio", false, "enable audio")
	argVideo := flag.Bool("video", false, "enable video")

	/*d
	argBenchPath := flag.String("o", "bench.csv", "output bench path")
	argBenchInterval := flag.Int("interval", 0, "bench sample interval")
	*/

	flag.Parse()

	gst.Init(nil)

	cancelChan := make(chan os.Signal)
	ctx, cancel := context.WithCancel(context.Background())

	now := time.Now()
	userID := now.UnixMilli() % (1 << 31)
	fmt.Printf("UserId: %+v\n", userID)

	roomID := int64(*argRoomID)
	url := fmt.Sprintf("ws://%s:%d/", *argHost, *argPort)

	benchPath := os.DevNull
	if *argBench {
		benchPath = fmt.Sprintf("bench-%d.csv", userID)
	}

	// WebRTC
	var engine webrtc.MediaEngine
	var registry interceptor.Registry
	api := createWebRTCAPI(&engine, &registry)

	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.
	pubPeerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	pubPeerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
	})
	// Create a audio track
	opusTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		panic(err)
	} else if _, err = pubPeerConnection.AddTrack(opusTrack); err != nil {
		panic(err)
	}

	// Create a video track
	vp8Track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/vp8"}, "video", "pion")
	if err != nil {
		panic(err)
	} else if _, err = pubPeerConnection.AddTrack(vp8Track); err != nil {
		panic(err)
	}

	offer, err := pubPeerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(pubPeerConnection)

	if err = pubPeerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	subPeerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// We must offer to send media for Janus to send anything
	if _, err := subPeerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		panic(err)
	} else if _, err = subPeerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		panic(err)
	}

	/* Create Janus */
	gateway, err := janus.Connect(url)
	if err != nil {
		panic(err)
	}

	session, err := gateway.Create()
	if err != nil {
		panic(err)
	}

	pubHandle, err := session.Attach("janus.plugin.videoroom")
	if err != nil {
		panic(err)
	}

	subHandle, err := session.Attach("janus.plugin.videoroom")
	if err != nil {
		panic(err)
	}

	// init bench csv
	benchFile, err := os.Create(benchPath)
	if err != nil {
		panic(err)
	}
	defer benchFile.Close()

	benchWriter := csv.NewWriter(benchFile)
	defer benchWriter.Flush()

	benchWriter.Write(BENCH_COLUMN_NAMES)

	subPeerConnection.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go saveBench(ctx, track, benchWriter, 5)

		codec := track.Codec()
		trackID := track.ID()
		fmt.Printf("[%d] Got TrackRemote %s(%s, rtx=%t)\n", subHandle.ID, trackID, codec.MimeType, track.HasRTX())
		if codec.MimeType == "audio/opus" {
			path := ""
			if *argAudio {
				path = fmt.Sprintf("audio-%d-%s.ogg", userID, trackID)
			}
			saveOpusToDisk(ctx, track, path)
		} else if codec.MimeType == "video/VP8" {
			path := ""
			if *argVideo {
				path = fmt.Sprintf("video-%d-%s.ivf", userID, trackID)
			}
			saveVP8ToDisk(ctx, track, path)
		} else {
			fmt.Printf("[%s] Unknown codec: %s\n", track.ID(), codec.MimeType)
		}
	})

	go func() {
		for {
			if _, keepAliveErr := session.KeepAlive(); keepAliveErr != nil {
				panic(keepAliveErr)
			}

			time.Sleep(5 * time.Second)
		}
	}()

	go watchHandle(subHandle)

	/* Init Publisher */
	joinMsg, err := pubHandle.Message(map[string]interface{}{
		"request": "join",
		"ptype":   "publisher",
		"room":    roomID,
		"id":      userID,
	}, nil)
	if err != nil {
		panic(err)
	}

	/* Init Subscriber */
	publishers := joinMsg.Plugindata.Data["publishers"]
	feeds, err := publishersToFeeds(publishers)
	if err != nil {
		panic(err)
	}
	go watchPubHandle(pubHandle, subPeerConnection, subHandle, roomID, feeds)

	msg, err := pubHandle.Message(map[string]interface{}{
		"request": "publish",
		"audio":   true,
		"video":   true,
		"data":    false,
	}, map[string]interface{}{
		"type":    "offer",
		"sdp":     pubPeerConnection.LocalDescription().SDP,
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
		err = pubPeerConnection.SetRemoteDescription(webrtc.SessionDescription{
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

	// start sigint notify
	signal.Notify(cancelChan, syscall.SIGINT, syscall.SIGTERM)

	// wait signal
	sig := <-cancelChan
	fmt.Printf("Got Signal %v: clean up...\n", sig)
	gateway.Close()
	cancel()
	wg.Wait()
	fmt.Printf("Done!\n")
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
	/* Does not work */
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
