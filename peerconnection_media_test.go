// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package webrtc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/transport/v3/test"
	"github.com/pion/transport/v3/vnet"
	"github.com/pion/webrtc/v4/internal/util"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errIncomingTrackIDInvalid    = errors.New("incoming Track ID is invalid")
	errIncomingTrackLabelInvalid = errors.New("incoming Track Label is invalid")
	errNoTransceiverwithMid      = errors.New("no transceiver with mid")
)

/*
Integration test for bi-directional peers

This asserts we can send RTP and RTCP both ways, and blocks until
each side gets something (and asserts payload contents)
*/
//nolint:gocyclo,cyclop
func TestPeerConnection_Media_Sample(t *testing.T) {
	const (
		expectedTrackID  = "video"
		expectedStreamID = "pion"
	)

	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcOffer, pcAnswer, err := newPair()
	assert.NoError(t, err)

	awaitRTPRecv := make(chan bool)
	awaitRTPRecvClosed := make(chan bool)
	awaitRTPSend := make(chan bool)

	awaitRTCPSenderRecv := make(chan bool)
	awaitRTCPSenderSend := make(chan error)

	awaitRTCPReceiverRecv := make(chan error)
	awaitRTCPReceiverSend := make(chan error)

	trackMetadataValid := make(chan error)

	pcAnswer.OnTrack(func(track *TrackRemote, receiver *RTPReceiver) {
		if track.ID() != expectedTrackID {
			trackMetadataValid <- fmt.Errorf(
				"%w: expected(%s) actual(%s)", errIncomingTrackIDInvalid, expectedTrackID, track.ID(),
			)

			return
		}

		if track.StreamID() != expectedStreamID {
			trackMetadataValid <- fmt.Errorf(
				"%w: expected(%s) actual(%s)", errIncomingTrackLabelInvalid, expectedStreamID, track.StreamID(),
			)

			return
		}
		close(trackMetadataValid)

		go func() {
			for {
				time.Sleep(time.Millisecond * 100)
				if routineErr := pcAnswer.WriteRTCP([]rtcp.Packet{&rtcp.RapidResynchronizationRequest{
					SenderSSRC: uint32(track.SSRC()), MediaSSRC: uint32(track.SSRC()),
				}}); routineErr != nil {
					awaitRTCPReceiverSend <- routineErr

					return
				}

				select {
				case <-awaitRTCPSenderRecv:
					close(awaitRTCPReceiverSend)

					return
				default:
				}
			}
		}()

		go func() {
			_, _, routineErr := receiver.Read(make([]byte, 1400))
			if routineErr != nil {
				awaitRTCPReceiverRecv <- routineErr
			} else {
				close(awaitRTCPReceiverRecv)
			}
		}()

		haveClosedAwaitRTPRecv := false
		for {
			p, _, routineErr := track.ReadRTP()
			if routineErr != nil {
				close(awaitRTPRecvClosed)

				return
			} else if bytes.Equal(p.Payload, []byte{0x10, 0x00}) && !haveClosedAwaitRTPRecv {
				haveClosedAwaitRTPRecv = true
				close(awaitRTPRecv)
			}
		}
	})

	vp8Track, err := NewTrackLocalStaticSample(
		RTPCodecCapability{MimeType: MimeTypeVP8}, expectedTrackID, expectedStreamID,
	)
	assert.NoError(t, err)
	sender, err := pcOffer.AddTrack(vp8Track)
	assert.NoError(t, err)

	go func() {
		for {
			time.Sleep(time.Millisecond * 100)
			if pcOffer.ICEConnectionState() != ICEConnectionStateConnected {
				continue
			}
			if routineErr := vp8Track.WriteSample(media.Sample{Data: []byte{0x00}, Duration: time.Second}); routineErr != nil {
				//nolint:forbidigo // not a test failure
				fmt.Println(routineErr)
			}

			select {
			case <-awaitRTPRecv:
				close(awaitRTPSend)

				return
			default:
			}
		}
	}()

	go func() {
		parameters := sender.GetParameters()

		<-awaitRTPSend
		for {
			time.Sleep(time.Millisecond * 100)
			if routineErr := pcOffer.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					SenderSSRC: uint32(parameters.Encodings[0].SSRC), MediaSSRC: uint32(parameters.Encodings[0].SSRC),
				},
			}); routineErr != nil {
				awaitRTCPSenderSend <- routineErr
			}

			select {
			case <-awaitRTCPReceiverRecv:
				close(awaitRTCPSenderSend)

				return
			default:
			}
		}
	}()

	go func() {
		if _, _, routineErr := sender.Read(make([]byte, 1400)); routineErr == nil {
			close(awaitRTCPSenderRecv)
		}
	}()

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	err, ok := <-trackMetadataValid
	assert.NoError(t, err)
	assert.False(t, ok)

	<-awaitRTPRecv
	<-awaitRTPSend

	<-awaitRTCPSenderRecv
	err, ok = <-awaitRTCPSenderSend
	assert.NoError(t, err)
	assert.False(t, ok)

	<-awaitRTCPReceiverRecv
	err, ok = <-awaitRTCPReceiverSend
	assert.NoError(t, err)
	assert.False(t, ok)

	closePairNow(t, pcOffer, pcAnswer)
	<-awaitRTPRecvClosed
}

// PeerConnection should be able to be torn down at anytime
// This test adds an input track and asserts
// OnTrack doesn't fire since no video packets will arrive
// No goroutine leaks
// No deadlocks on shutdown.
func TestPeerConnection_Media_Shutdown(t *testing.T) { //nolint:cyclop
	iceCompleteAnswer := make(chan struct{})
	iceCompleteOffer := make(chan struct{})

	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcOffer, pcAnswer, err := newPair()
	assert.NoError(t, err)

	_, err = pcOffer.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	_, err = pcAnswer.AddTransceiverFromKind(
		RTPCodecTypeAudio,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	opusTrack, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeOpus}, "audio", "pion1")
	assert.NoError(t, err)

	vp8Track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2")
	assert.NoError(t, err)

	_, err = pcOffer.AddTrack(opusTrack)
	assert.NoError(t, err)
	_, err = pcAnswer.AddTrack(vp8Track)
	assert.NoError(t, err)

	var onTrackFiredLock sync.Mutex
	onTrackFired := false

	pcAnswer.OnTrack(func(*TrackRemote, *RTPReceiver) {
		onTrackFiredLock.Lock()
		defer onTrackFiredLock.Unlock()
		onTrackFired = true
	})

	pcAnswer.OnICEConnectionStateChange(func(iceState ICEConnectionState) {
		if iceState == ICEConnectionStateConnected {
			close(iceCompleteAnswer)
		}
	})
	pcOffer.OnICEConnectionStateChange(func(iceState ICEConnectionState) {
		if iceState == ICEConnectionStateConnected {
			close(iceCompleteOffer)
		}
	})

	err = signalPair(pcOffer, pcAnswer)
	assert.NoError(t, err)
	<-iceCompleteAnswer
	<-iceCompleteOffer

	// Each PeerConnection should have one sender, one receiver and one transceiver
	for _, pc := range []*PeerConnection{pcOffer, pcAnswer} {
		senders := pc.GetSenders()
		assert.Len(t, senders, 1, "Each PeerConnection should have one RTPSender")

		receivers := pc.GetReceivers()
		assert.Len(t, receivers, 2, "Each PeerConnection should have two RTPReceivers")

		transceivers := pc.GetTransceivers()
		assert.Len(t, transceivers, 2, "Each PeerConnection should have two RTPTransceivers")
	}

	closePairNow(t, pcOffer, pcAnswer)

	onTrackFiredLock.Lock()
	assert.False(t, onTrackFired, "PeerConnection OnTrack fired even though we got no packets")
	onTrackFiredLock.Unlock()
}

// Integration test for behavior around media and disconnected peers
// Sending RTP and RTCP to a disconnected Peer shouldn't return an error.

func TestPeerConnection_Media_Disconnected(t *testing.T) { //nolint:cyclop
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	s := SettingEngine{}
	s.SetICETimeouts(time.Second/2, time.Second/2, time.Second/8)

	mediaEngine := &MediaEngine{}
	assert.NoError(t, mediaEngine.RegisterDefaultCodecs())

	pcOffer, pcAnswer, wan := createVNetPair(t, nil)

	keepPackets := &atomic.Bool{}
	keepPackets.Store(true)

	// Add a filter that monitors the traffic on the router
	wan.AddChunkFilter(func(vnet.Chunk) bool {
		return keepPackets.Load()
	})

	vp8Track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2")
	assert.NoError(t, err)

	vp8Sender, err := pcOffer.AddTrack(vp8Track)
	assert.NoError(t, err)

	haveDisconnected := make(chan error)
	pcOffer.OnICEConnectionStateChange(func(iceState ICEConnectionState) {
		if iceState == ICEConnectionStateDisconnected {
			close(haveDisconnected)
		} else if iceState == ICEConnectionStateConnected {
			// Assert that DTLS is done by pull remote certificate, don't tear down the PC early
			for {
				if len(vp8Sender.Transport().GetRemoteCertificate()) != 0 {
					if pcAnswer.sctpTransport.association() != nil {
						break
					}
				}

				time.Sleep(time.Second)
			}

			keepPackets.Store(false)
		}
	})

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	err, ok := <-haveDisconnected
	assert.False(t, ok)
	assert.NoError(t, err)
	for i := 0; i <= 5; i++ {
		err = vp8Track.WriteSample(media.Sample{Data: []byte{0x00}, Duration: time.Second})
		assert.NoError(t, err)
		err = pcOffer.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: 0}})
		assert.NoError(t, err)
	}

	assert.NoError(t, wan.Stop())
	closePairNow(t, pcOffer, pcAnswer)
}

type undeclaredSsrcLogger struct{ unhandledSimulcastError chan struct{} }

func (u *undeclaredSsrcLogger) Trace(string)          {}
func (u *undeclaredSsrcLogger) Tracef(string, ...any) {}
func (u *undeclaredSsrcLogger) Debug(string)          {}
func (u *undeclaredSsrcLogger) Debugf(string, ...any) {}
func (u *undeclaredSsrcLogger) Info(string)           {}
func (u *undeclaredSsrcLogger) Infof(string, ...any)  {}
func (u *undeclaredSsrcLogger) Warn(string)           {}
func (u *undeclaredSsrcLogger) Warnf(string, ...any)  {}
func (u *undeclaredSsrcLogger) Error(string)          {}
func (u *undeclaredSsrcLogger) Errorf(format string, _ ...any) {
	if format == incomingUnhandledRTPSsrc {
		close(u.unhandledSimulcastError)
	}
}

type undeclaredSsrcLoggerFactory struct{ unhandledSimulcastError chan struct{} }

func (u *undeclaredSsrcLoggerFactory) NewLogger(string) logging.LeveledLogger {
	return &undeclaredSsrcLogger{u.unhandledSimulcastError}
}

// Filter SSRC lines.
func filterSsrc(offer string) (filteredSDP string) {
	scanner := bufio.NewScanner(strings.NewReader(offer))
	for scanner.Scan() {
		l := scanner.Text()
		if strings.HasPrefix(l, "a=ssrc") {
			continue
		}

		filteredSDP += l + "\n"
	}

	return
}

func filterSDPExtensions(offer string) (filteredSDP string) {
	scanner := bufio.NewScanner(strings.NewReader(offer))
	for scanner.Scan() {
		l := scanner.Text()
		if strings.HasPrefix(l, "a=extmap") {
			continue
		}

		filteredSDP += l + "\n"
	}

	return
}

// If a SessionDescription has a single media section and no SSRC
// assume that it is meant to handle all RTP packets.
func TestUndeclaredSSRC(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	t.Run("No SSRC", func(t *testing.T) {
		pcOffer, pcAnswer, err := newPair()
		assert.NoError(t, err)

		vp8Writer, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2")
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(vp8Writer)
		assert.NoError(t, err)

		onTrackFired := make(chan struct{})
		pcAnswer.OnTrack(func(trackRemote *TrackRemote, _ *RTPReceiver) {
			assert.Equal(t, trackRemote.StreamID(), vp8Writer.StreamID())
			assert.Equal(t, trackRemote.ID(), vp8Writer.ID())
			close(onTrackFired)
		})

		offer, err := pcOffer.CreateOffer(nil)
		assert.NoError(t, err)

		offerGatheringComplete := GatheringCompletePromise(pcOffer)
		assert.NoError(t, pcOffer.SetLocalDescription(offer))
		<-offerGatheringComplete

		offer.SDP = filterSsrc(pcOffer.LocalDescription().SDP)
		assert.NoError(t, pcAnswer.SetRemoteDescription(offer))

		answer, err := pcAnswer.CreateAnswer(nil)
		assert.NoError(t, err)

		answerGatheringComplete := GatheringCompletePromise(pcAnswer)
		assert.NoError(t, pcAnswer.SetLocalDescription(answer))
		<-answerGatheringComplete

		assert.NoError(t, pcOffer.SetRemoteDescription(*pcAnswer.LocalDescription()))

		sendVideoUntilDone(t, onTrackFired, []*TrackLocalStaticSample{vp8Writer})
		closePairNow(t, pcOffer, pcAnswer)
	})

	t.Run("Has RID", func(t *testing.T) {
		unhandledSimulcastError := make(chan struct{})

		mediaEngine := &MediaEngine{}
		assert.NoError(t, mediaEngine.RegisterDefaultCodecs())

		pcOffer, pcAnswer, err := NewAPI(WithSettingEngine(SettingEngine{
			LoggerFactory: &undeclaredSsrcLoggerFactory{unhandledSimulcastError},
		}), WithMediaEngine(mediaEngine)).newPair(Configuration{})
		assert.NoError(t, err)

		vp8Writer, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2")
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(vp8Writer)
		assert.NoError(t, err)

		offer, err := pcOffer.CreateOffer(nil)
		assert.NoError(t, err)

		offerGatheringComplete := GatheringCompletePromise(pcOffer)
		assert.NoError(t, pcOffer.SetLocalDescription(offer))
		<-offerGatheringComplete

		// Append RID to end of SessionDescription. Will not be considered unhandled anymore
		offer.SDP = filterSsrc(pcOffer.LocalDescription().SDP) + "a=" + sdpAttributeRid + "\r\n"
		assert.NoError(t, pcAnswer.SetRemoteDescription(offer))

		answer, err := pcAnswer.CreateAnswer(nil)
		assert.NoError(t, err)

		answerGatheringComplete := GatheringCompletePromise(pcAnswer)
		assert.NoError(t, pcAnswer.SetLocalDescription(answer))
		<-answerGatheringComplete

		assert.NoError(t, pcOffer.SetRemoteDescription(*pcAnswer.LocalDescription()))

		sendVideoUntilDone(t, unhandledSimulcastError, []*TrackLocalStaticSample{vp8Writer})
		closePairNow(t, pcOffer, pcAnswer)
	})

	t.Run("multiple media sections, no sdp extensions", func(t *testing.T) {
		pcOffer, pcAnswer, err := newPair()
		assert.NoError(t, err)

		vp8Writer, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion")
		assert.NoError(t, err)

		_, err = pcOffer.CreateDataChannel("data", nil)
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(vp8Writer)
		assert.NoError(t, err)

		opusWriter, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeOpus}, "audio", "pion")
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(opusWriter)
		assert.NoError(t, err)

		onVideoTrackFired := make(chan struct{})
		onAudioTrackFired := make(chan struct{})

		gotVideo, gotAudio := false, false
		pcAnswer.OnTrack(func(trackRemote *TrackRemote, _ *RTPReceiver) {
			switch trackRemote.Kind() {
			case RTPCodecTypeVideo:
				assert.False(t, gotVideo, "already got video track")
				assert.Equal(t, trackRemote.StreamID(), vp8Writer.StreamID())
				assert.Equal(t, trackRemote.ID(), vp8Writer.ID())
				gotVideo = true
				onVideoTrackFired <- struct{}{}
			case RTPCodecTypeAudio:
				assert.False(t, gotAudio, "already got audio track")
				assert.Equal(t, trackRemote.StreamID(), opusWriter.StreamID())
				assert.Equal(t, trackRemote.ID(), opusWriter.ID())
				gotAudio = true
				onAudioTrackFired <- struct{}{}
			default:
				assert.Fail(t, "unexpected track kind", trackRemote.Kind())
			}
		})

		offer, err := pcOffer.CreateOffer(nil)
		assert.NoError(t, err)

		offerGatheringComplete := GatheringCompletePromise(pcOffer)
		assert.NoError(t, pcOffer.SetLocalDescription(offer))
		<-offerGatheringComplete

		offer.SDP = filterSDPExtensions(filterSsrc(pcOffer.LocalDescription().SDP))
		assert.NoError(t, pcAnswer.SetRemoteDescription(offer))

		answer, err := pcAnswer.CreateAnswer(nil)
		assert.NoError(t, err)

		answerGatheringComplete := GatheringCompletePromise(pcAnswer)
		assert.NoError(t, pcAnswer.SetLocalDescription(answer))
		<-answerGatheringComplete

		assert.NoError(t, pcOffer.SetRemoteDescription(*pcAnswer.LocalDescription()))

		wait := sync.WaitGroup{}
		wait.Add(2)
		go func() {
			sendVideoUntilDone(t, onVideoTrackFired, []*TrackLocalStaticSample{vp8Writer})
			wait.Done()
		}()
		go func() {
			sendVideoUntilDone(t, onAudioTrackFired, []*TrackLocalStaticSample{opusWriter})
			wait.Done()
		}()

		wait.Wait()
		closePairNow(t, pcOffer, pcAnswer)
	})

	t.Run("findMediaSectionByPayloadType test", func(t *testing.T) {
		parsed := &SessionDescription{
			parsed: &sdp.SessionDescription{
				MediaDescriptions: []*sdp.MediaDescription{
					{
						MediaName: sdp.MediaName{
							Media:   "video",
							Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
							Formats: []string{"96", "97", "98", "99", "BAD", "100", "101", "102"},
						},
					},
					{
						MediaName: sdp.MediaName{
							Media:   "audio",
							Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
							Formats: []string{"8", "9", "101"},
						},
					},
					{
						MediaName: sdp.MediaName{
							Media:   "application",
							Protos:  []string{"UDP", "DTLS", "SCTP"},
							Formats: []string{"webrtc-datachannel"},
						},
					},
				},
			},
		}
		peer := &PeerConnection{}

		video, ok := peer.findMediaSectionByPayloadType(96, parsed)
		assert.True(t, ok)
		assert.NotNil(t, video)
		assert.Equal(t, "video", video.MediaName.Media)

		audio, ok := peer.findMediaSectionByPayloadType(8, parsed)
		assert.True(t, ok)
		assert.NotNil(t, audio)
		assert.Equal(t, "audio", audio.MediaName.Media)

		missing, ok := peer.findMediaSectionByPayloadType(42, parsed)
		assert.False(t, ok)
		assert.Nil(t, missing)
	})
}

func TestAddTransceiverFromTrackSendOnly(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	track, err := NewTrackLocalStaticSample(
		RTPCodecCapability{MimeType: "audio/Opus"},
		"track-id",
		"stream-id",
	)
	assert.NoError(t, err)

	transceiver, err := pc.AddTransceiverFromTrack(track, RTPTransceiverInit{
		Direction: RTPTransceiverDirectionSendonly,
	})
	assert.NoError(t, err)

	assert.Nil(t, transceiver.Receiver(), "Transceiver shouldn't have a receiver")
	assert.NotNil(t, transceiver.Sender(), "Transceiver should have a sender")
	assert.Len(t, pc.GetTransceivers(), 1, "PeerConnection should have one transceiver")
	assert.Len(t, pc.GetSenders(), 1, "PeerConnection should have one sender")

	offer, err := pc.CreateOffer(nil)
	assert.NoError(t, err)

	assert.Truef(
		t, offerMediaHasDirection(offer, RTPCodecTypeAudio, RTPTransceiverDirectionSendonly),
		"Direction on SDP is not %s", RTPTransceiverDirectionSendonly,
	)

	assert.NoError(t, pc.Close())
}

func TestAddTransceiverFromTrackSendRecv(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	track, err := NewTrackLocalStaticSample(
		RTPCodecCapability{MimeType: "audio/Opus"},
		"track-id",
		"stream-id",
	)
	assert.NoError(t, err)

	transceiver, err := pc.AddTransceiverFromTrack(track, RTPTransceiverInit{
		Direction: RTPTransceiverDirectionSendrecv,
	})
	assert.NoError(t, err)
	assert.NotNil(t, transceiver.Receiver(), "Transceiver should have a receiver")
	assert.NotNil(t, transceiver.Sender(), "Transceiver should have a sender")
	assert.Len(t, pc.GetTransceivers(), 1, "PeerConnection should have one transceiver")

	offer, err := pc.CreateOffer(nil)
	assert.NoError(t, err)

	assert.Truef(
		t, offerMediaHasDirection(offer, RTPCodecTypeAudio, RTPTransceiverDirectionSendrecv),
		"Direction on SDP is not %s", RTPTransceiverDirectionSendrecv,
	)
	assert.NoError(t, pc.Close())
}

func TestAddTransceiverAddTrack_Reuse(t *testing.T) {
	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	tr, err := pc.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	assert.Equal(t, []*RTPTransceiver{tr}, pc.GetTransceivers())

	addTrack := func() (TrackLocal, *RTPSender) {
		track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "foo", "bar")
		assert.NoError(t, err)

		sender, err := pc.AddTrack(track)
		assert.NoError(t, err)

		return track, sender
	}

	track1, sender1 := addTrack()
	assert.Equal(t, 1, len(pc.GetTransceivers()))
	assert.Equal(t, sender1, tr.Sender())
	assert.Equal(t, track1, tr.Sender().Track())
	require.NoError(t, pc.RemoveTrack(sender1))

	track2, _ := addTrack()
	assert.Equal(t, 1, len(pc.GetTransceivers()))
	assert.Equal(t, track2, tr.Sender().Track())

	addTrack()
	assert.Equal(t, 2, len(pc.GetTransceivers()))

	assert.NoError(t, pc.Close())
}

func TestAddTransceiverAddTrack_NewRTPSender_Error(t *testing.T) {
	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	_, err = pc.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	dtlsTransport := pc.dtlsTransport
	pc.dtlsTransport = nil

	track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "foo", "bar")
	assert.NoError(t, err)

	_, err = pc.AddTrack(track)
	assert.Error(t, err, "DTLSTransport must not be nil")

	assert.Equal(t, 1, len(pc.GetTransceivers()))

	pc.dtlsTransport = dtlsTransport
	assert.NoError(t, pc.Close())
}

func TestRtpSenderReceiver_ReadClose_Error(t *testing.T) {
	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	tr, err := pc.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionSendrecv},
	)
	assert.NoError(t, err)

	sender, receiver := tr.Sender(), tr.Receiver()
	assert.NoError(t, sender.Stop())
	_, _, err = sender.Read(make([]byte, 0, 1400))
	assert.ErrorIs(t, err, io.ErrClosedPipe)

	assert.NoError(t, receiver.Stop())
	_, _, err = receiver.Read(make([]byte, 0, 1400))
	assert.ErrorIs(t, err, io.ErrClosedPipe)

	assert.NoError(t, pc.Close())
}

// nolint: dupl
func TestAddTransceiverFromKind(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	transceiver, err := pc.AddTransceiverFromKind(RTPCodecTypeVideo, RTPTransceiverInit{
		Direction: RTPTransceiverDirectionRecvonly,
	})
	assert.NoError(t, err)

	assert.NotNil(t, transceiver.Receiver(), "Transceiver should have a receiver")
	assert.Nil(t, transceiver.Sender(), "Transceiver shouldn't have a sender")

	offer, err := pc.CreateOffer(nil)
	assert.NoError(t, err)

	assert.Truef(
		t, offerMediaHasDirection(offer, RTPCodecTypeVideo, RTPTransceiverDirectionRecvonly),
		"Direction on SDP is not %s", RTPTransceiverDirectionRecvonly,
	)
	assert.NoError(t, pc.Close())
}

func TestAddTransceiverFromTrackFailsRecvOnly(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	track, err := NewTrackLocalStaticSample(
		RTPCodecCapability{
			MimeType:    MimeTypeH264,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		"track-id",
		"track-label",
	)
	assert.NoError(t, err)

	transceiver, err := pc.AddTransceiverFromTrack(track, RTPTransceiverInit{
		Direction: RTPTransceiverDirectionRecvonly,
	})

	assert.Nil(
		t, transceiver,
		"AddTransceiverFromTrack shouldn't succeed with Direction RTPTransceiverDirectionRecvonly",
	)

	assert.NotNil(t, err)
	assert.NoError(t, pc.Close())
}

func TestPlanBMediaExchange(t *testing.T) {
	runTest := func(t *testing.T, trackCount int) {
		t.Helper()

		addSingleTrack := func(p *PeerConnection) *TrackLocalStaticSample {
			track, err := NewTrackLocalStaticSample(
				RTPCodecCapability{MimeType: MimeTypeVP8},
				fmt.Sprintf("video-%d", util.RandUint32()),
				fmt.Sprintf("video-%d", util.RandUint32()),
			)
			assert.NoError(t, err)

			_, err = p.AddTrack(track)
			assert.NoError(t, err)

			return track
		}

		pcOffer, err := NewPeerConnection(Configuration{SDPSemantics: SDPSemanticsPlanB})
		assert.NoError(t, err)

		pcAnswer, err := NewPeerConnection(Configuration{SDPSemantics: SDPSemanticsPlanB})
		assert.NoError(t, err)

		var onTrackWaitGroup sync.WaitGroup
		onTrackWaitGroup.Add(trackCount)
		pcAnswer.OnTrack(func(*TrackRemote, *RTPReceiver) {
			onTrackWaitGroup.Done()
		})

		done := make(chan struct{})
		go func() {
			onTrackWaitGroup.Wait()
			close(done)
		}()

		_, err = pcAnswer.AddTransceiverFromKind(RTPCodecTypeVideo)
		assert.NoError(t, err)

		outboundTracks := []*TrackLocalStaticSample{}
		for i := 0; i < trackCount; i++ {
			outboundTracks = append(outboundTracks, addSingleTrack(pcOffer))
		}

		assert.NoError(t, signalPair(pcOffer, pcAnswer))

		func() {
			for {
				select {
				case <-time.After(20 * time.Millisecond):
					for _, track := range outboundTracks {
						assert.NoError(t, track.WriteSample(media.Sample{Data: []byte{0x00}, Duration: time.Second}))
					}
				case <-done:
					return
				}
			}
		}()

		closePairNow(t, pcOffer, pcAnswer)
	}

	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	t.Run("Single Track", func(t *testing.T) {
		runTest(t, 1)
	})
	t.Run("Multi Track", func(t *testing.T) {
		runTest(t, 2)
	})
}

// TestPeerConnection_Start_Only_Negotiated_Senders tests that only
// the current negotiated transceivers senders provided in an
// offer/answer are started.
func TestPeerConnection_Start_Only_Negotiated_Senders(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcOffer, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)
	defer func() { assert.NoError(t, pcOffer.Close()) }()

	pcAnswer, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)
	defer func() { assert.NoError(t, pcAnswer.Close()) }()

	track1, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion1")
	require.NoError(t, err)

	sender1, err := pcOffer.AddTrack(track1)
	require.NoError(t, err)

	offer, err := pcOffer.CreateOffer(nil)
	assert.NoError(t, err)

	offerGatheringComplete := GatheringCompletePromise(pcOffer)
	assert.NoError(t, pcOffer.SetLocalDescription(offer))
	<-offerGatheringComplete
	assert.NoError(t, pcAnswer.SetRemoteDescription(*pcOffer.LocalDescription()))
	answer, err := pcAnswer.CreateAnswer(nil)
	assert.NoError(t, err)
	answerGatheringComplete := GatheringCompletePromise(pcAnswer)
	assert.NoError(t, pcAnswer.SetLocalDescription(answer))
	<-answerGatheringComplete

	// Add a new track between providing the offer and applying the answer

	track2, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2")
	require.NoError(t, err)

	sender2, err := pcOffer.AddTrack(track2)
	require.NoError(t, err)

	// apply answer so we'll test generateMatchedSDP
	assert.NoError(t, pcOffer.SetRemoteDescription(*pcAnswer.LocalDescription()))

	// Wait for senders to be started by startTransports spawned goroutine
	pcOffer.ops.Done()

	// sender1 should be started but sender2 should not be started
	assert.True(t, sender1.hasSent(), "sender1 is not started but should be started")
	assert.False(t, sender2.hasSent(), "sender2 is started but should not be started")
}

// TestPeerConnection_Start_Right_Receiver tests that the right
// receiver (the receiver which transceiver has the same media section as the track)
// is started for the specified track.
func TestPeerConnection_Start_Right_Receiver(t *testing.T) {
	isTransceiverReceiverStarted := func(pc *PeerConnection, mid string) (bool, error) {
		for _, transceiver := range pc.GetTransceivers() {
			if transceiver.Mid() != mid {
				continue
			}

			return transceiver.Receiver() != nil && transceiver.Receiver().haveReceived(), nil
		}

		return false, fmt.Errorf("%w: %q", errNoTransceiverwithMid, mid)
	}

	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcOffer, pcAnswer, err := newPair()
	require.NoError(t, err)

	_, err = pcAnswer.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	track1, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion1")
	require.NoError(t, err)

	sender1, err := pcOffer.AddTrack(track1)
	require.NoError(t, err)

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	pcOffer.ops.Done()
	pcAnswer.ops.Done()

	// transceiver with mid 0 should be started
	started, err := isTransceiverReceiverStarted(pcAnswer, "0")
	assert.NoError(t, err)
	assert.True(t, started, "transceiver with mid 0 should be started")

	// Remove track
	assert.NoError(t, pcOffer.RemoveTrack(sender1))

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	pcOffer.ops.Done()
	pcAnswer.ops.Done()

	// transceiver with mid 0 should not be started
	started, err = isTransceiverReceiverStarted(pcAnswer, "0")
	assert.NoError(t, err)
	assert.False(t, started, "transceiver with mid 0 should not be started")

	// Add a new transceiver (we're not using AddTrack since it'll reuse the transceiver with mid 0)
	_, err = pcOffer.AddTransceiverFromTrack(track1)
	assert.NoError(t, err)

	_, err = pcAnswer.AddTransceiverFromKind(
		RTPCodecTypeVideo,
		RTPTransceiverInit{Direction: RTPTransceiverDirectionRecvonly},
	)
	assert.NoError(t, err)

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	pcOffer.ops.Done()
	pcAnswer.ops.Done()

	// transceiver with mid 0 should not be started
	started, err = isTransceiverReceiverStarted(pcAnswer, "0")
	assert.NoError(t, err)
	assert.False(t, started, "transceiver with mid 0 should not be started")
	// transceiver with mid 2 should be started
	started, err = isTransceiverReceiverStarted(pcAnswer, "2")
	assert.NoError(t, err)
	assert.True(t, started, "transceiver with mid 2 should be started")

	closePairNow(t, pcOffer, pcAnswer)
}

func TestPeerConnection_Simulcast_Probe(t *testing.T) { //nolint:cyclop
	lim := test.TimeOut(time.Second * 30) //nolint
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	// Assert that failed Simulcast probing doesn't cause
	// the handleUndeclaredSSRC to be leaked
	t.Run("Leak", func(t *testing.T) {
		track, err := NewTrackLocalStaticRTP(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion")
		assert.NoError(t, err)

		offerer, answerer, err := newPair()
		assert.NoError(t, err)

		_, err = offerer.AddTrack(track)
		assert.NoError(t, err)

		ticker := time.NewTicker(time.Millisecond * 20)
		defer ticker.Stop()
		testFinished := make(chan struct{})
		seenFiveStreams, seenFiveStreamsCancel := context.WithCancel(context.Background())

		go func() {
			for {
				select {
				case <-testFinished:
					return
				case <-ticker.C:
					answerer.dtlsTransport.lock.Lock()
					if len(answerer.dtlsTransport.simulcastStreams) >= 5 {
						seenFiveStreamsCancel()
					}
					answerer.dtlsTransport.lock.Unlock()

					track.mu.Lock()
					if len(track.bindings) == 1 {
						_, err = track.bindings[0].writeStream.WriteRTP(&rtp.Header{
							Version: 2,
							SSRC:    util.RandUint32(),
						}, []byte{0, 1, 2, 3, 4, 5})
						assert.NoError(t, err)
					}
					track.mu.Unlock()
				}
			}
		}()

		assert.NoError(t, signalPair(offerer, answerer))

		peerConnectionConnected := untilConnectionState(PeerConnectionStateConnected, offerer, answerer)
		peerConnectionConnected.Wait()

		<-seenFiveStreams.Done()

		closePairNow(t, offerer, answerer)
		close(testFinished)
	})

	// Assert that NonSimulcast Traffic isn't incorrectly broken by the probe
	t.Run("Break NonSimulcast", func(t *testing.T) {
		unhandledSimulcastError := make(chan struct{})

		mediaEngine := &MediaEngine{}
		assert.NoError(t, mediaEngine.RegisterDefaultCodecs())
		assert.NoError(t, ConfigureSimulcastExtensionHeaders(mediaEngine))

		pcOffer, pcAnswer, err := NewAPI(WithSettingEngine(SettingEngine{
			LoggerFactory: &undeclaredSsrcLoggerFactory{unhandledSimulcastError},
		}), WithMediaEngine(mediaEngine)).newPair(Configuration{})
		assert.NoError(t, err)

		firstTrack, err := NewTrackLocalStaticRTP(RTPCodecCapability{MimeType: MimeTypeVP8}, "firstTrack", "firstTrack")
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(firstTrack)
		assert.NoError(t, err)

		secondTrack, err := NewTrackLocalStaticRTP(RTPCodecCapability{MimeType: MimeTypeVP8}, "secondTrack", "secondTrack")
		assert.NoError(t, err)

		_, err = pcOffer.AddTrack(secondTrack)
		assert.NoError(t, err)

		assert.NoError(t, signalPairWithModification(pcOffer, pcAnswer, func(sessionDescription string) (filtered string) {
			shouldDiscard := false

			scanner := bufio.NewScanner(strings.NewReader(sessionDescription))
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "m=video") {
					shouldDiscard = !shouldDiscard
				} else if strings.HasPrefix(scanner.Text(), "a=group:BUNDLE") {
					filtered += "a=group:BUNDLE 1 2\r\n"

					continue
				}

				if !shouldDiscard {
					filtered += scanner.Text() + "\r\n"
				}
			}

			return
		}))

		peerConnectionConnected := untilConnectionState(PeerConnectionStateConnected, pcOffer, pcAnswer)
		peerConnectionConnected.Wait()

		sequenceNumber := uint16(0)
		sendRTPPacket := func() {
			sequenceNumber++
			assert.NoError(t, firstTrack.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequenceNumber,
				},
				Payload: []byte{0x00},
			}))
			time.Sleep(20 * time.Millisecond)
		}

		for ; sequenceNumber <= 5; sequenceNumber++ {
			sendRTPPacket()
		}

		trackRemoteChan := make(chan *TrackRemote, 1)
		pcAnswer.OnTrack(func(trackRemote *TrackRemote, _ *RTPReceiver) {
			trackRemoteChan <- trackRemote
		})

		assert.NoError(t, signalPair(pcOffer, pcAnswer))

		trackRemote := func() *TrackRemote {
			for {
				select {
				case t := <-trackRemoteChan:
					return t
				default:
					sendRTPPacket()
				}
			}
		}()

		func() {
			for {
				select {
				case <-unhandledSimulcastError:
					return
				default:
					sendRTPPacket()
				}
			}
		}()

		_, _, err = trackRemote.Read(make([]byte, 1500))
		assert.NoError(t, err)

		closePairNow(t, pcOffer, pcAnswer)
	})
}

// Assert that CreateOffer returns an error for a RTPSender with no codecs
// pion/webrtc#1702
// .
func TestPeerConnection_CreateOffer_NoCodecs(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	mediaEngine := &MediaEngine{}

	pc, err := NewAPI(WithMediaEngine(mediaEngine)).NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	track, err := NewTrackLocalStaticRTP(RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion")
	assert.NoError(t, err)

	_, err = pc.AddTrack(track)
	assert.NoError(t, err)

	_, err = pc.CreateOffer(nil)
	assert.Equal(t, err, ErrSenderWithNoCodecs)

	assert.NoError(t, pc.Close())
}

// Assert that AddTrack is thread-safe.
func TestPeerConnection_RaceReplaceTrack(t *testing.T) {
	pc, err := NewPeerConnection(Configuration{})
	assert.NoError(t, err)

	addTrack := func() *TrackLocalStaticSample {
		track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "foo", "bar")
		assert.NoError(t, err)
		_, err = pc.AddTrack(track)
		assert.NoError(t, err)

		return track
	}

	for i := 0; i < 10; i++ {
		addTrack()
	}
	for _, tr := range pc.GetTransceivers() {
		assert.NoError(t, pc.RemoveTrack(tr.Sender()))
	}

	var wg sync.WaitGroup
	tracks := make([]*TrackLocalStaticSample, 10)
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go func(j int) {
			tracks[j] = addTrack()
			wg.Done()
		}(i)
	}

	wg.Wait()

	for _, track := range tracks {
		have := false
		for _, t := range pc.GetTransceivers() {
			if t.Sender() != nil && t.Sender().Track() == track {
				have = true

				break
			}
		}
		assert.True(t, have, "track was added but not found on senders")
	}

	assert.NoError(t, pc.Close())
}

func TestPeerConnection_Simulcast(t *testing.T) { //nolint:cyclop
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	rids := []string{"a", "b", "c"}

	t.Run("E2E", func(t *testing.T) {
		pcOffer, pcAnswer, err := newPair()
		assert.NoError(t, err)

		vp8WriterA, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[0]),
		)
		assert.NoError(t, err)

		vp8WriterB, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[1]),
		)
		assert.NoError(t, err)

		vp8WriterC, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[2]),
		)
		assert.NoError(t, err)

		sender, err := pcOffer.AddTrack(vp8WriterA)
		assert.NoError(t, err)
		assert.NotNil(t, sender)

		assert.NoError(t, sender.AddEncoding(vp8WriterB))
		assert.NoError(t, sender.AddEncoding(vp8WriterC))

		var ridMapLock sync.RWMutex
		ridMap := map[string]int{}

		assertRidCorrect := func(t *testing.T) {
			t.Helper()

			ridMapLock.Lock()
			defer ridMapLock.Unlock()

			for _, rid := range rids {
				assert.Equal(t, ridMap[rid], 1)
			}
			assert.Equal(t, len(ridMap), 3)
		}

		ridsFullfilled := func() bool {
			ridMapLock.Lock()
			defer ridMapLock.Unlock()

			ridCount := len(ridMap)

			return ridCount == 3
		}

		pcAnswer.OnTrack(func(trackRemote *TrackRemote, _ *RTPReceiver) {
			ridMapLock.Lock()
			defer ridMapLock.Unlock()
			ridMap[trackRemote.RID()] = ridMap[trackRemote.RID()] + 1
		})

		parameters := sender.GetParameters()
		assert.Equal(t, "a", parameters.Encodings[0].RID)
		assert.Equal(t, "b", parameters.Encodings[1].RID)
		assert.Equal(t, "c", parameters.Encodings[2].RID)

		var midID, ridID uint8
		for _, extension := range parameters.HeaderExtensions {
			switch extension.URI {
			case sdp.SDESMidURI:
				midID = uint8(extension.ID) //nolint:gosec // G115
			case sdp.SDESRTPStreamIDURI:
				ridID = uint8(extension.ID) //nolint:gosec // G115
			}
		}
		assert.NotZero(t, midID)
		assert.NotZero(t, ridID)

		assert.NoError(t, signalPair(pcOffer, pcAnswer))

		// padding only packets should not affect simulcast probe
		var sequenceNumber uint16
		for sequenceNumber = 0; sequenceNumber < simulcastProbeCount+10; sequenceNumber++ {
			time.Sleep(20 * time.Millisecond)

			for _, track := range []*TrackLocalStaticRTP{vp8WriterA, vp8WriterB, vp8WriterC} {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						SequenceNumber: sequenceNumber,
						PayloadType:    96,
						Padding:        true,
					},
					Payload: []byte{0x00, 0x02},
				}

				assert.NoError(t, track.WriteRTP(pkt))
			}
		}
		assert.False(t, ridsFullfilled(), "Simulcast probe should not be fulfilled by padding only packets")

		for ; !ridsFullfilled(); sequenceNumber++ {
			time.Sleep(20 * time.Millisecond)

			for _, track := range []*TrackLocalStaticRTP{vp8WriterA, vp8WriterB, vp8WriterC} {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						SequenceNumber: sequenceNumber,
						PayloadType:    96,
					},
					Payload: []byte{0x00},
				}
				assert.NoError(t, pkt.Header.SetExtension(midID, []byte("0")))
				assert.NoError(t, pkt.Header.SetExtension(ridID, []byte(track.RID())))

				assert.NoError(t, track.WriteRTP(pkt))
			}
		}

		assertRidCorrect(t)
		closePairNow(t, pcOffer, pcAnswer)
	})

	t.Run("RTCP", func(t *testing.T) {
		pcOffer, pcAnswer, err := newPair()
		assert.NoError(t, err)

		vp8WriterA, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[0]),
		)
		assert.NoError(t, err)

		vp8WriterB, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[1]),
		)
		assert.NoError(t, err)

		vp8WriterC, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[2]),
		)
		assert.NoError(t, err)

		sender, err := pcOffer.AddTrack(vp8WriterA)
		assert.NoError(t, err)
		assert.NotNil(t, sender)

		assert.NoError(t, sender.AddEncoding(vp8WriterB))
		assert.NoError(t, sender.AddEncoding(vp8WriterC))

		rtcpCounter := uint64(0)
		pcAnswer.OnTrack(func(trackRemote *TrackRemote, receiver *RTPReceiver) {
			_, _, simulcastReadErr := receiver.ReadSimulcastRTCP(trackRemote.RID())
			assert.NoError(t, simulcastReadErr)
			atomic.AddUint64(&rtcpCounter, 1)
		})

		var midID, ridID uint8
		for _, extension := range sender.GetParameters().HeaderExtensions {
			switch extension.URI {
			case sdp.SDESMidURI:
				midID = uint8(extension.ID) //nolint:gosec // G115
			case sdp.SDESRTPStreamIDURI:
				ridID = uint8(extension.ID) //nolint:gosec // G115
			}
		}
		assert.NotZero(t, midID)
		assert.NotZero(t, ridID)

		assert.NoError(t, signalPair(pcOffer, pcAnswer))

		for sequenceNumber := uint16(0); atomic.LoadUint64(&rtcpCounter) < 3; sequenceNumber++ {
			time.Sleep(20 * time.Millisecond)

			for _, track := range []*TrackLocalStaticRTP{vp8WriterA, vp8WriterB, vp8WriterC} {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						SequenceNumber: sequenceNumber,
						PayloadType:    96,
					},
					Payload: []byte{0x00},
				}
				assert.NoError(t, pkt.Header.SetExtension(midID, []byte("0")))
				assert.NoError(t, pkt.Header.SetExtension(ridID, []byte(track.RID())))

				assert.NoError(t, track.WriteRTP(pkt))
			}
		}

		closePairNow(t, pcOffer, pcAnswer)
	})
}

type simulcastTestTrackLocal struct {
	*TrackLocalStaticRTP
}

// don't use ssrc&payload in bindings to let the test write different stream packets.
func (s *simulcastTestTrackLocal) WriteRTP(pkt *rtp.Packet) error {
	packet := getPacketAllocationFromPool()

	defer resetPacketPoolAllocation(packet)

	*packet = *pkt

	s.mu.RLock()
	defer s.mu.RUnlock()

	writeErrs := []error{}

	for _, b := range s.bindings {
		if _, err := b.writeStream.WriteRTP(&packet.Header, packet.Payload); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	return util.FlattenErrs(writeErrs)
}

func TestPeerConnection_Simulcast_RTX(t *testing.T) { //nolint:cyclop
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	rids := []string{"a", "b"}
	pcOffer, pcAnswer, err := newPair()
	assert.NoError(t, err)

	vp8WriterAStatic, err := NewTrackLocalStaticRTP(
		RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[0]),
	)
	assert.NoError(t, err)

	vp8WriterBStatic, err := NewTrackLocalStaticRTP(
		RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion2", WithRTPStreamID(rids[1]),
	)
	assert.NoError(t, err)

	vp8WriterA, vp8WriterB := &simulcastTestTrackLocal{vp8WriterAStatic}, &simulcastTestTrackLocal{vp8WriterBStatic}

	sender, err := pcOffer.AddTrack(vp8WriterA)
	assert.NoError(t, err)
	assert.NotNil(t, sender)

	assert.NoError(t, sender.AddEncoding(vp8WriterB))

	var ridMapLock sync.RWMutex
	ridMap := map[string]int{}

	assertRidCorrect := func(t *testing.T) {
		t.Helper()

		ridMapLock.Lock()
		defer ridMapLock.Unlock()

		for _, rid := range rids {
			assert.Equal(t, ridMap[rid], 1)
		}
		assert.Equal(t, len(ridMap), 2)
	}

	ridsFullfilled := func() bool {
		ridMapLock.Lock()
		defer ridMapLock.Unlock()

		ridCount := len(ridMap)

		return ridCount == 2
	}

	var rtxPacketRead atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)

	pcAnswer.OnTrack(func(trackRemote *TrackRemote, _ *RTPReceiver) {
		ridMapLock.Lock()
		ridMap[trackRemote.RID()] = ridMap[trackRemote.RID()] + 1
		ridMapLock.Unlock()

		defer wg.Done()

		for {
			_, attr, rerr := trackRemote.ReadRTP()
			if rerr != nil {
				break
			}
			if pt, ok := attr.Get(AttributeRtxPayloadType).(byte); ok {
				if pt == 97 {
					rtxPacketRead.Add(1)
				}
			}
		}
	})

	parameters := sender.GetParameters()
	assert.Equal(t, "a", parameters.Encodings[0].RID)
	assert.Equal(t, "b", parameters.Encodings[1].RID)

	var midID, ridID, rsid uint8
	for _, extension := range parameters.HeaderExtensions {
		switch extension.URI {
		case sdp.SDESMidURI:
			midID = uint8(extension.ID) //nolint:gosec // G115
		case sdp.SDESRTPStreamIDURI:
			ridID = uint8(extension.ID) //nolint:gosec // G115
		case sdp.SDESRepairRTPStreamIDURI:
			rsid = uint8(extension.ID) //nolint:gosec // G115
		}
	}
	assert.NotZero(t, midID)
	assert.NotZero(t, ridID)
	assert.NotZero(t, rsid)

	err = signalPairWithModification(pcOffer, pcAnswer, func(sdp string) string {
		// Original chrome sdp contains no ssrc info https://pastebin.com/raw/JTjX6zg6
		re := regexp.MustCompile("(?m)[\r\n]+^.*a=ssrc.*$")
		res := re.ReplaceAllString(sdp, "")

		return res
	})
	assert.NoError(t, err)

	// padding only packets should not affect simulcast probe
	var sequenceNumber uint16
	for sequenceNumber = 0; sequenceNumber < simulcastProbeCount+10; sequenceNumber++ {
		time.Sleep(20 * time.Millisecond)

		for i, track := range []*simulcastTestTrackLocal{vp8WriterA, vp8WriterB} {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequenceNumber,
					PayloadType:    96,
					Padding:        true,
					SSRC:           uint32(i + 1), //nolint:gosec // G115
				},
				Payload: []byte{0x00, 0x02},
			}

			assert.NoError(t, track.WriteRTP(pkt))
		}
	}
	assert.False(t, ridsFullfilled(), "Simulcast probe should not be fulfilled by padding only packets")

	for ; !ridsFullfilled(); sequenceNumber++ {
		time.Sleep(20 * time.Millisecond)

		for i, track := range []*simulcastTestTrackLocal{vp8WriterA, vp8WriterB} {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequenceNumber,
					PayloadType:    96,
					SSRC:           uint32(i + 1), //nolint:gosec // G115
				},
				Payload: []byte{0x00},
			}
			assert.NoError(t, pkt.Header.SetExtension(midID, []byte("0")))
			assert.NoError(t, pkt.Header.SetExtension(ridID, []byte(track.RID())))

			assert.NoError(t, track.WriteRTP(pkt))
		}
	}

	assertRidCorrect(t)

	for i := 0; i < simulcastProbeCount+10; i++ {
		sequenceNumber++
		time.Sleep(10 * time.Millisecond)

		for j, track := range []*simulcastTestTrackLocal{vp8WriterA, vp8WriterB} {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequenceNumber,
					PayloadType:    97,
					SSRC:           uint32(100 + j), //nolint:gosec // G115
				},
				Payload: []byte{0x00, 0x00, 0x00, 0x00, 0x00},
			}
			assert.NoError(t, pkt.Header.SetExtension(midID, []byte("0")))
			assert.NoError(t, pkt.Header.SetExtension(ridID, []byte(track.RID())))
			assert.NoError(t, pkt.Header.SetExtension(rsid, []byte(track.RID())))

			assert.NoError(t, track.WriteRTP(pkt))
		}
	}

	for ; rtxPacketRead.Load() == 0; sequenceNumber++ {
		time.Sleep(20 * time.Millisecond)

		for i, track := range []*simulcastTestTrackLocal{vp8WriterA, vp8WriterB} {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: sequenceNumber,
					PayloadType:    96,
					SSRC:           uint32(i + 1), //nolint:gosec // G115
				},
				Payload: []byte{0x00},
			}
			assert.NoError(t, pkt.Header.SetExtension(midID, []byte("0")))
			assert.NoError(t, pkt.Header.SetExtension(ridID, []byte(track.RID())))

			assert.NoError(t, track.WriteRTP(pkt))
		}
	}

	closePairNow(t, pcOffer, pcAnswer)

	wg.Wait()

	assert.Greater(t, rtxPacketRead.Load(), int32(0), "no rtx packet read")
}

// Everytime we receive a new SSRC we probe it and try to determine the proper way to handle it.
// In most cases a Track explicitly declares a SSRC and a OnTrack is fired. In two cases we don't
// know the SSRC ahead of time
// * Undeclared SSRC in a single media section (https://github.com/pion/webrtc/issues/880)
// * Simulcast
//
// The Undeclared SSRC processing code would run before Simulcast. If a Simulcast Offer/Answer only
// contained one Media Section we would never fire the OnTrack. We would assume it was a failed
// Undeclared SSRC processing. This test asserts that we properly handled this.
func TestPeerConnection_Simulcast_NoDataChannel(t *testing.T) {
	lim := test.TimeOut(time.Second * 30)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcSender, pcReceiver, err := newPair()
	assert.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(4)

	var connectionWg sync.WaitGroup
	connectionWg.Add(2)

	connectionStateChangeHandler := func(state PeerConnectionState) {
		if state == PeerConnectionStateConnected {
			connectionWg.Done()
		}
	}

	pcSender.OnConnectionStateChange(connectionStateChangeHandler)
	pcReceiver.OnConnectionStateChange(connectionStateChangeHandler)

	pcReceiver.OnTrack(func(*TrackRemote, *RTPReceiver) {
		defer wg.Done()
	})

	go func() {
		defer wg.Done()
		vp8WriterA, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion", WithRTPStreamID("a"),
		)
		assert.NoError(t, err)

		sender, err := pcSender.AddTrack(vp8WriterA)
		assert.NoError(t, err)
		assert.NotNil(t, sender)

		vp8WriterB, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion", WithRTPStreamID("b"),
		)
		assert.NoError(t, err)
		err = sender.AddEncoding(vp8WriterB)
		assert.NoError(t, err)

		vp8WriterC, err := NewTrackLocalStaticRTP(
			RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion", WithRTPStreamID("c"),
		)
		assert.NoError(t, err)
		err = sender.AddEncoding(vp8WriterC)
		assert.NoError(t, err)

		parameters := sender.GetParameters()
		var midID, ridID, rsidID uint8
		for _, extension := range parameters.HeaderExtensions {
			switch extension.URI {
			case sdp.SDESMidURI:
				midID = uint8(extension.ID) //nolint:gosec // G115
			case sdp.SDESRTPStreamIDURI:
				ridID = uint8(extension.ID) //nolint:gosec // G115
			case sdp.SDESRepairRTPStreamIDURI:
				rsidID = uint8(extension.ID) //nolint:gosec // G115
			}
		}
		assert.NotZero(t, midID)
		assert.NotZero(t, ridID)
		assert.NotZero(t, rsidID)

		// signaling
		offerSDP, err := pcSender.CreateOffer(nil)
		assert.NoError(t, err)
		err = pcSender.SetLocalDescription(offerSDP)
		assert.NoError(t, err)

		err = pcReceiver.SetRemoteDescription(offerSDP)
		assert.NoError(t, err)
		answerSDP, err := pcReceiver.CreateAnswer(nil)
		assert.NoError(t, err)

		answerGatheringComplete := GatheringCompletePromise(pcReceiver)
		err = pcReceiver.SetLocalDescription(answerSDP)
		assert.NoError(t, err)
		<-answerGatheringComplete

		assert.NoError(t, pcSender.SetRemoteDescription(*pcReceiver.LocalDescription()))

		connectionWg.Wait()

		var seqNo uint16
		for i := 0; i < 100; i++ {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					SequenceNumber: seqNo,
					PayloadType:    96,
				},
				Payload: []byte{0x00, 0x00},
			}

			assert.NoError(t, pkt.SetExtension(ridID, []byte("a")))
			assert.NoError(t, pkt.SetExtension(midID, []byte(sender.rtpTransceiver.Mid())))
			assert.NoError(t, vp8WriterA.WriteRTP(pkt))

			assert.NoError(t, pkt.SetExtension(ridID, []byte("b")))
			assert.NoError(t, pkt.SetExtension(midID, []byte(sender.rtpTransceiver.Mid())))
			assert.NoError(t, vp8WriterB.WriteRTP(pkt))

			assert.NoError(t, pkt.SetExtension(ridID, []byte("c")))
			assert.NoError(t, pkt.SetExtension(midID, []byte(sender.rtpTransceiver.Mid())))
			assert.NoError(t, vp8WriterC.WriteRTP(pkt))

			seqNo++
		}
	}()

	wg.Wait()

	closePairNow(t, pcSender, pcReceiver)
}

// Check that PayloadType of 0 is handled correctly. At one point
// we incorrectly assumed 0 meant an invalid stream and wouldn't update things
// properly.
func TestPeerConnection_Zero_PayloadType(t *testing.T) {
	lim := test.TimeOut(time.Second * 5)
	defer lim.Stop()

	report := test.CheckRoutines(t)
	defer report()

	pcOffer, pcAnswer, err := newPair()
	require.NoError(t, err)

	audioTrack, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypePCMU}, "audio", "audio")
	require.NoError(t, err)

	_, err = pcOffer.AddTrack(audioTrack)
	require.NoError(t, err)

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	trackFired := make(chan struct{})

	pcAnswer.OnTrack(func(track *TrackRemote, _ *RTPReceiver) {
		require.Equal(t, track.Codec().MimeType, MimeTypePCMU)
		close(trackFired)
	})

	func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-trackFired:
				return
			case <-ticker.C:
				if routineErr := audioTrack.WriteSample(
					media.Sample{Data: []byte{0x00}, Duration: time.Second},
				); routineErr != nil {
					//nolint:forbidigo // not a test failure
					fmt.Println(routineErr)
				}
			}
		}
	}()

	closePairNow(t, pcOffer, pcAnswer)
}

// Assert that NACKs work E2E with no extra configuration. If media is sent over a lossy connection
// the user gets retransmitted RTP packets with no extra configuration.
func Test_PeerConnection_RTX_E2E(t *testing.T) { //nolint:cyclop
	defer test.TimeOut(time.Second * 30).Stop()

	pcOffer, pcAnswer, wan := createVNetPair(t, nil)

	wan.AddChunkFilter(func(vnet.Chunk) bool {
		return rand.Intn(5) != 4 //nolint: gosec
	})

	track, err := NewTrackLocalStaticSample(RTPCodecCapability{MimeType: MimeTypeVP8}, "track-id", "stream-id")
	assert.NoError(t, err)

	rtpSender, err := pcOffer.AddTrack(track)
	assert.NoError(t, err)

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	rtxSsrc := rtpSender.GetParameters().Encodings[0].RTX.SSRC
	ssrc := rtpSender.GetParameters().Encodings[0].SSRC

	rtxRead, rtxReadCancel := context.WithCancel(context.Background())
	pcAnswer.OnTrack(func(track *TrackRemote, _ *RTPReceiver) {
		for {
			pkt, attributes, readRTPErr := track.ReadRTP()
			if errors.Is(readRTPErr, io.EOF) {
				return
			} else if pkt.PayloadType == 0 {
				continue
			}

			assert.NotNil(t, pkt)
			assert.Equal(t, pkt.SSRC, uint32(ssrc))
			assert.Equal(t, pkt.PayloadType, uint8(96))

			rtxPayloadType := attributes.Get(AttributeRtxPayloadType)
			rtxSequenceNumber := attributes.Get(AttributeRtxSequenceNumber)
			rtxSSRC := attributes.Get(AttributeRtxSsrc)
			if rtxPayloadType != nil && rtxSequenceNumber != nil && rtxSSRC != nil {
				assert.Equal(t, rtxPayloadType, uint8(97))
				assert.Equal(t, rtxSSRC, uint32(rtxSsrc))

				rtxReadCancel()
			}
		}
	})

	assert.NoError(t, signalPair(pcOffer, pcAnswer))

	func() {
		for {
			select {
			case <-time.After(20 * time.Millisecond):
				writeErr := track.WriteSample(media.Sample{Data: []byte{0x00}, Duration: time.Second})
				assert.NoError(t, writeErr)
			case <-rtxRead.Done():
				return
			}
		}
	}()

	assert.NoError(t, wan.Stop())
	closePairNow(t, pcOffer, pcAnswer)
}
