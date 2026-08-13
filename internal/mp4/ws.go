package mp4

import (
	"errors"
	"strings"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
)

func handlerWSMSE(tr *ws.Transport, msg *ws.Message) error {
	stream, _ := streams.GetOrPatch(tr.Request.URL.Query())
	if stream == nil {
		return errors.New(api.StreamNotFound)
	}

	var medias []*core.Media
	if codecs := msg.String(); codecs != "" {
		log.Trace().Str("codecs", codecs).Msgf("[mp4] new WS/MSE consumer")
		medias = mp4.ParseCodecs(codecs, true)
	}

	cons := mp4.NewConsumer(medias)
	cons.FormatName = "mse/fmp4"
	cons.WithRequest(tr.Request)

	if err := stream.AddConsumer(cons); err != nil {
		log.Debug().Err(err).Msg("[mp4] add consumer")
		return err
	}

	// Metadata rotation/scale via the fMP4 track matrix (no re-encode), so a
	// stream whose source can't rotate (e.g. the Ingenic-T7 Petkit reading
	// already-encoded H.264) can still be shown upright. Must be set before
	// WriteTo generates the init segment. Matches the /api/stream.mp4 handler;
	// honored by Chrome/Firefox MSE (not Safari).
	query := tr.Request.URL.Query()
	if rotate := query.Get("rotate"); rotate != "" {
		cons.Rotate = core.Atoi(rotate)
	}
	if scale := query.Get("scale"); scale != "" {
		if sx, sy, ok := strings.Cut(scale, ":"); ok {
			cons.ScaleX = core.Atoi(sx)
			cons.ScaleY = core.Atoi(sy)
		}
	}

	tr.Write(&ws.Message{Type: "mse", Value: mp4.ContentType(cons.Codecs())})

	go cons.WriteTo(tr.Writer())

	tr.OnClose(func() {
		stream.RemoveConsumer(cons)
	})

	return nil
}

func handlerWSMP4(tr *ws.Transport, msg *ws.Message) error {
	stream, _ := streams.GetOrPatch(tr.Request.URL.Query())
	if stream == nil {
		return errors.New(api.StreamNotFound)
	}

	var medias []*core.Media
	if codecs := msg.String(); codecs != "" {
		log.Trace().Str("codecs", codecs).Msgf("[mp4] new WS/MP4 consumer")
		medias = mp4.ParseCodecs(codecs, false)
	}

	cons := mp4.NewKeyframe(medias)
	cons.WithRequest(tr.Request)

	if err := stream.AddConsumer(cons); err != nil {
		log.Error().Err(err).Caller().Send()
		return err
	}

	tr.Write(&ws.Message{Type: "mse", Value: mp4.ContentType(cons.Codecs())})

	go cons.WriteTo(tr.Writer())

	tr.OnClose(func() {
		stream.RemoveConsumer(cons)
	})

	return nil
}
