package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ledgerwatch/log/v3"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ledgerwatch/erigon-lib/chain/snapcfg"
	"github.com/ledgerwatch/erigon-lib/common/background"
	"github.com/ledgerwatch/erigon-lib/downloader/snaptype"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/p2p/sentry/simulator"
	"github.com/ledgerwatch/erigon/polygon/heimdall"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync/freezeblocks"
)

type HeimdallSimulator struct {
	ctx                context.Context
	knownBorSnapshots  *freezeblocks.BorRoSnapshots
	activeBorSnapshots *freezeblocks.BorRoSnapshots
	blockReader        *freezeblocks.BlockReader
	logger             log.Logger
	downloader         *simulator.TorrentClient

	lastDownloadedBlockNumber uint64
}

type IndexFnType func(context.Context, snaptype.FileInfo, uint32, string, *background.Progress, log.Lvl, log.Logger) error

func NewHeimdall(ctx context.Context, chain string, snapshotLocation string, logger log.Logger) (HeimdallSimulator, error) {
	cfg := snapcfg.KnownCfg(chain)

	knownBorSnapshots := freezeblocks.NewBorRoSnapshots(ethconfig.Defaults.Snapshot, snapshotLocation, 0, logger)

	files := make([]string, 0, len(cfg.Preverified))

	for _, item := range cfg.Preverified {
		files = append(files, item.Name)
	}

	err := knownBorSnapshots.InitSegments(files)
	if err != nil {
		return HeimdallSimulator{}, err
	}

	activeBorSnapshots := freezeblocks.NewBorRoSnapshots(ethconfig.Defaults.Snapshot, snapshotLocation, 0, logger)

	if err := activeBorSnapshots.ReopenFolder(); err != nil {
		return HeimdallSimulator{}, err
	}

	downloader, err := simulator.NewTorrentClient(ctx, chain, snapshotLocation, logger)
	if err != nil {
		return HeimdallSimulator{}, err
	}

	s := HeimdallSimulator{
		ctx:                       ctx,
		knownBorSnapshots:         knownBorSnapshots,
		activeBorSnapshots:        activeBorSnapshots,
		blockReader:               freezeblocks.NewBlockReader(nil, activeBorSnapshots),
		logger:                    logger,
		downloader:                downloader,
		lastDownloadedBlockNumber: 0,
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	return s, nil
}

func (h *HeimdallSimulator) FetchLatestSpan(ctx context.Context) (*heimdall.Span, error) {
	latestSpan, _, err := h.blockReader.LastSpanId(ctx, nil)
	if err != nil {
		return nil, err
	}

	span, err := h.getSpan(h.ctx, latestSpan)
	if err != nil {
		return nil, err
	}

	return &span, nil
}

func (h *HeimdallSimulator) FetchSpan(ctx context.Context, spanID uint64) (*heimdall.Span, error) {
	span, err := h.getSpan(h.ctx, spanID)
	if err != nil {
		return nil, err
	}

	return &span, err
}

func (h *HeimdallSimulator) FetchStateSyncEvents(ctx context.Context, fromId uint64, to time.Time, limit int) ([]*heimdall.EventRecordWithTime, error) {
	events, maxTime, err := h.blockReader.EventsByIdFromSnapshot(fromId, to, limit)
	if err != nil {
		return nil, err
	}

	view := h.knownBorSnapshots.View()
	defer view.Close()

	for !maxTime && len(events) != limit {
		if seg, ok := view.EventsSegment(h.lastDownloadedBlockNumber); ok {
			if err := h.downloadData(ctx, seg, snaptype.BorEvents, freezeblocks.BorEventsIdx); err != nil {
				return nil, err
			}
		}
		h.lastDownloadedBlockNumber += 500000

		events, maxTime, err = h.blockReader.EventsByIdFromSnapshot(fromId, to, limit)
		if err != nil {
			return nil, err
		}
	}

	return events, nil
}

func (h *HeimdallSimulator) FetchCheckpoint(ctx context.Context, number int64) (*heimdall.Checkpoint, error) {
	return nil, status.Errorf(codes.Unimplemented, "method FetchCheckpoint not implemented")
}

func (h *HeimdallSimulator) FetchCheckpointCount(ctx context.Context) (int64, error) {
	return 0, status.Errorf(codes.Unimplemented, "method FetchCheckpointCount not implemented")
}

func (h *HeimdallSimulator) FetchMilestone(ctx context.Context, number int64) (*heimdall.Milestone, error) {
	return nil, status.Errorf(codes.Unimplemented, "method FetchMilestone not implemented")
}

func (h *HeimdallSimulator) FetchMilestoneCount(ctx context.Context) (int64, error) {
	return 0, status.Errorf(codes.Unimplemented, "method FetchMilestoneCount not implemented")
}

func (h *HeimdallSimulator) FetchNoAckMilestone(ctx context.Context, milestoneID string) error {
	return status.Errorf(codes.Unimplemented, "method FetchNoAckMilestone not implemented")
}

func (h *HeimdallSimulator) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	return "", status.Errorf(codes.Unimplemented, "method FetchLastNoAckMilestone not implemented")
}

func (h *HeimdallSimulator) FetchMilestoneID(ctx context.Context, milestoneID string) error {
	return status.Errorf(codes.Unimplemented, "method FetchMilestoneID not implemented")
}

func (h *HeimdallSimulator) Close() {
	_ = h.downloader.Close()

	h.activeBorSnapshots.Close()
	h.knownBorSnapshots.Close()
}

func (h *HeimdallSimulator) downloadData(ctx context.Context, spans *freezeblocks.Segment, sType snaptype.Type, indexFn IndexFnType) error {
	fileName := snaptype.SegmentFileName(1, spans.From(), spans.To(), sType.Enum())

	if slices.Contains(h.activeBorSnapshots.Files(), fileName) {
		return h.activeBorSnapshots.ReopenSegments([]snaptype.Type{sType}, true)
	}

	h.logger.Info(fmt.Sprintf("Downloading %s", fileName))

	err := h.downloader.Download(ctx, fileName)
	if err != nil {
		return fmt.Errorf("can't download %s: %w", fileName, err)
	}

	h.logger.Info(fmt.Sprintf("Indexing %s", fileName))

	info, _, _ := snaptype.ParseFileName(h.downloader.LocalFsRoot(), fileName)

	err = indexFn(ctx, info, h.activeBorSnapshots.Salt, h.downloader.LocalFsRoot(), nil, log.LvlWarn, h.logger)
	if err != nil {
		return fmt.Errorf("can't download %s: %w", fileName, err)
	}

	return h.activeBorSnapshots.ReopenSegments([]snaptype.Type{sType}, true)
}

func (h *HeimdallSimulator) getSpan(ctx context.Context, spanId uint64) (heimdall.Span, error) {
	span, err := h.blockReader.Span(ctx, nil, spanId)
	if span != nil && err == nil {
		var s heimdall.Span
		if err = json.Unmarshal(span, &s); err != nil {
			return heimdall.Span{}, err
		}
		return s, err
	}

	if span == nil {
		view := h.knownBorSnapshots.View()
		defer view.Close()

		if seg, ok := view.SpansSegment(spanId); ok {
			if err := h.downloadData(ctx, seg, snaptype.BorSpans, freezeblocks.BorSpansIdx); err != nil {
				return heimdall.Span{}, err
			}
		}

		span, err = h.blockReader.Span(ctx, nil, spanId)
		if err != nil {
			return heimdall.Span{}, err
		}
	}

	var s heimdall.Span
	if err := json.Unmarshal(span, &s); err != nil {
		return heimdall.Span{}, err
	}

	return s, nil
}
