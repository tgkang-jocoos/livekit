package connectionquality

import (
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	UpdateInterval                   = 5 * time.Second
	noReceiverReportTooLongThreshold = 30 * time.Second
)

type ConnectionStatsParams struct {
	UpdateInterval            time.Duration
	MimeType                  string
	IsFECEnabled              bool
	IncludeRTT                bool
	IncludeJitter             bool
	GetDeltaStats             func() map[uint32]*buffer.StreamStatsWithLayers
	GetDeltaStatsOverridden   func() map[uint32]*buffer.StreamStatsWithLayers
	GetLastReceiverReportTime func() time.Time
	Logger                    logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	isStarted atomic.Bool
	isVideo   atomic.Bool

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock               sync.RWMutex
	streamingStartedAt time.Time

	scorer *qualityScorer

	done core.Fuse
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		scorer: newQualityScorer(qualityScorerParams{
			PacketLossWeight: getPacketLossWeight(params.MimeType, params.IsFECEnabled), // LK-TODO: have to notify codec change?
			IncludeRTT:       params.IncludeRTT,
			IncludeJitter:    params.IncludeJitter,
			Logger:           params.Logger,
		}),
		done: core.NewFuse(),
	}
}

func (cs *ConnectionStats) start(trackInfo *livekit.TrackInfo) {
	cs.isVideo.Store(trackInfo.Type == livekit.TrackType_VIDEO)
	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) StartAt(trackInfo *livekit.TrackInfo, at time.Time) {
	if cs.isStarted.Swap(true) {
		return
	}

	cs.scorer.StartAt(at)
	cs.start(trackInfo)
}

func (cs *ConnectionStats) Start(trackInfo *livekit.TrackInfo) {
	if cs.isStarted.Swap(true) {
		return
	}

	cs.scorer.Start()
	cs.start(trackInfo)
}

func (cs *ConnectionStats) Close() {
	cs.done.Break()
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) UpdateMuteAt(isMuted bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateMuteAt(isMuted, at)
}

func (cs *ConnectionStats) UpdateMute(isMuted bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateMute(isMuted)
}

func (cs *ConnectionStats) AddBitrateTransitionAt(bitrate int64, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddBitrateTransitionAt(bitrate, at)
}

func (cs *ConnectionStats) AddBitrateTransition(bitrate int64) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddBitrateTransition(bitrate)
}

func (cs *ConnectionStats) UpdateLayerMuteAt(isMuted bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateLayerMuteAt(isMuted, at)
}

func (cs *ConnectionStats) UpdateLayerMute(isMuted bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateLayerMute(isMuted)
}

func (cs *ConnectionStats) UpdatePauseAt(isPaused bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdatePauseAt(isPaused, at)
}

func (cs *ConnectionStats) UpdatePause(isPaused bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdatePause(isPaused)
}

func (cs *ConnectionStats) AddLayerTransitionAt(distance float64, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddLayerTransitionAt(distance, at)
}

func (cs *ConnectionStats) AddLayerTransition(distance float64) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddLayerTransition(distance)
}

func (cs *ConnectionStats) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return cs.scorer.GetMOSAndQuality()
}

func (cs *ConnectionStats) updateScoreWithAggregate(agg *buffer.RTPDeltaInfo, at time.Time) float32 {
	var stat windowStat
	if agg != nil {
		stat.startedAt = agg.StartTime
		stat.duration = agg.Duration
		stat.packetsExpected = agg.Packets + agg.PacketsPadding
		stat.packetsLost = agg.PacketsLost
		stat.packetsMissing = agg.PacketsMissing
		stat.packetsOutOfOrder = agg.PacketsOutOfOrder
		stat.bytes = agg.Bytes - agg.HeaderBytes // only use media payload size
		stat.rttMax = agg.RttMax
		stat.jitterMax = agg.JitterMax
	}
	if at.IsZero() {
		cs.scorer.Update(&stat)
	} else {
		cs.scorer.UpdateAt(&stat, at)
	}

	mos, _ := cs.scorer.GetMOSAndQuality()
	return mos
}

func (cs *ConnectionStats) updateScoreFromReceiverReport(at time.Time) (float32, map[uint32]*buffer.StreamStatsWithLayers) {
	if cs.params.GetDeltaStatsOverridden == nil || cs.params.GetLastReceiverReportTime == nil {
		return MinMOS, nil
	}

	cs.lock.RLock()
	streamingStartedAt := cs.streamingStartedAt
	cs.lock.RUnlock()
	if streamingStartedAt.IsZero() {
		// not streaming, just return current score
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil
	}

	streams := cs.params.GetDeltaStatsOverridden()
	if len(streams) == 0 {
		//  check for receiver report not received for a while
		marker := cs.params.GetLastReceiverReportTime()
		if marker.IsZero() || streamingStartedAt.After(marker) {
			marker = streamingStartedAt
		}
		if time.Since(marker) > noReceiverReportTooLongThreshold {
			// have not received receiver report for a long time when streaming, run with nil stat
			return cs.updateScoreWithAggregate(nil, at), nil
		}

		// wait for receiver report, return current score
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil
	}

	// delta stat duration could be large due to not receiving receiver report for a long time (for example, due to mute),
	// adjust to streaming start if necessary
	agg := toAggregateDeltaInfo(streams)
	if streamingStartedAt.After(cs.params.GetLastReceiverReportTime()) {
		// last receiver report was before streaming started, wait for next one
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, streams
	}

	if streamingStartedAt.After(agg.StartTime) {
		agg.Duration = agg.StartTime.Add(agg.Duration).Sub(streamingStartedAt)
		agg.StartTime = streamingStartedAt
	}
	return cs.updateScoreWithAggregate(agg, at), streams
}

func (cs *ConnectionStats) updateScoreAt(at time.Time) (float32, map[uint32]*buffer.StreamStatsWithLayers) {
	if cs.params.GetDeltaStats == nil {
		return MinMOS, nil
	}

	streams := cs.params.GetDeltaStats()
	if len(streams) == 0 {
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil
	}

	deltaInfoList := make([]*buffer.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		deltaInfoList = append(deltaInfoList, s.RTPStats)
	}
	agg := buffer.AggregateRTPDeltaInfo(deltaInfoList)
	if agg != nil && agg.Packets > 0 {
		// not very accurate as streaming could have started part way in the window, but don't need accurate time
		cs.maybeSetStreamingStart(agg.StartTime)
	} else {
		cs.clearStreamingStart()
	}

	if cs.params.GetDeltaStatsOverridden != nil {
		// receiver report based quality scoring, use stats from receiver report for scoring
		return cs.updateScoreFromReceiverReport(at)
	}

	return cs.updateScoreWithAggregate(agg, at), streams
}

func (cs *ConnectionStats) maybeSetStreamingStart(at time.Time) {
	cs.lock.Lock()
	if cs.streamingStartedAt.IsZero() {
		cs.streamingStartedAt = at
	}
	cs.lock.Unlock()
}

func (cs *ConnectionStats) clearStreamingStart() {
	cs.lock.Lock()
	cs.streamingStartedAt = time.Time{}
	cs.lock.Unlock()
}

func (cs *ConnectionStats) getStat() {
	score, streams := cs.updateScoreAt(time.Time{})

	if cs.onStatsUpdate != nil && len(streams) != 0 {
		analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
		for ssrc, stream := range streams {
			as := toAnalyticsStream(ssrc, stream.RTPStats)

			//
			// add video layer if either
			//   1. Simulcast - even if there is only one layer per stream as it provides layer id
			//   2. A stream has multiple layers
			//
			if (len(streams) > 1 || len(stream.Layers) > 1) && cs.isVideo.Load() {
				for layer, layerStats := range stream.Layers {
					avl := toAnalyticsVideoLayer(layer, layerStats)
					if avl != nil {
						as.VideoLayers = append(as.VideoLayers, avl)
					}
				}
			}

			analyticsStreams = append(analyticsStreams, as)
		}

		cs.onStatsUpdate(cs, &livekit.AnalyticsStat{
			Score:   score,
			Streams: analyticsStreams,
			Mime:    cs.params.MimeType,
		})
	}
}

func (cs *ConnectionStats) updateStatsWorker() {
	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	for {
		select {
		case <-cs.done.Watch():
			return

		case <-tk.C:
			if cs.done.IsBroken() {
				return
			}

			cs.getStat()
		}
	}
}

// -----------------------------------------------------------------------

// how much weight to give to packet loss rate when calculating score.
// It is codec dependent.
// For audio:
//
//	o Opus without FEC or RED suffers the most through packet loss, hence has the highest weight
//	o RED with two packet redundancy can absorb two out of every three packets lost, so packet loss is not as detrimental and therefore lower weight
//
// For video:
//
//	o No in-built codec repair available, hence same for all codecs
func getPacketLossWeight(mimeType string, isFecEnabled bool) float64 {
	var plw float64
	switch {
	case strings.EqualFold(mimeType, webrtc.MimeTypeOpus):
		// 2.5%: fall to GOOD, 7.5%: fall to POOR
		plw = 8.0
		if isFecEnabled {
			// 3.75%: fall to GOOD, 11.25%: fall to POOR
			plw /= 1.5
		}

	case strings.EqualFold(mimeType, "audio/red"):
		// 10%: fall to GOOD, 30.0%: fall to POOR
		plw = 2.0
		if isFecEnabled {
			// 15%: fall to GOOD, 45.0%: fall to POOR
			plw /= 1.5
		}

	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		// 2%: fall to GOOD, 6%: fall to POOR
		plw = 10.0
	}

	return plw
}

func toAggregateDeltaInfo(streams map[uint32]*buffer.StreamStatsWithLayers) *buffer.RTPDeltaInfo {
	deltaInfoList := make([]*buffer.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		deltaInfoList = append(deltaInfoList, s.RTPStats)
	}
	return buffer.AggregateRTPDeltaInfo(deltaInfoList)
}

func toAnalyticsStream(ssrc uint32, deltaStats *buffer.RTPDeltaInfo) *livekit.AnalyticsStream {
	// discount the feed side loss when reporting forwarded track stats
	packetsLost := deltaStats.PacketsLost
	if deltaStats.PacketsMissing > packetsLost {
		packetsLost = 0
	} else {
		packetsLost -= deltaStats.PacketsMissing
	}
	return &livekit.AnalyticsStream{
		Ssrc:              ssrc,
		PrimaryPackets:    deltaStats.Packets,
		PrimaryBytes:      deltaStats.Bytes,
		RetransmitPackets: deltaStats.PacketsDuplicate,
		RetransmitBytes:   deltaStats.BytesDuplicate,
		PaddingPackets:    deltaStats.PacketsPadding,
		PaddingBytes:      deltaStats.BytesPadding,
		PacketsLost:       packetsLost,
		Frames:            deltaStats.Frames,
		Rtt:               deltaStats.RttMax,
		Jitter:            uint32(deltaStats.JitterMax),
		Nacks:             deltaStats.Nacks,
		Plis:              deltaStats.Plis,
		Firs:              deltaStats.Firs,
	}
}

func toAnalyticsVideoLayer(layer int32, layerStats *buffer.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
	avl := &livekit.AnalyticsVideoLayer{
		Layer:   layer,
		Packets: layerStats.Packets + layerStats.PacketsDuplicate + layerStats.PacketsPadding,
		Bytes:   layerStats.Bytes + layerStats.BytesDuplicate + layerStats.BytesPadding,
		Frames:  layerStats.Frames,
	}
	if avl.Packets == 0 || avl.Bytes == 0 || avl.Frames == 0 {
		return nil
	}

	return avl
}
