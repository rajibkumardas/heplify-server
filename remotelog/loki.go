package remotelog

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/negbie/heplify-server"
	"github.com/negbie/heplify-server/config"
	"github.com/negbie/logp"
	"github.com/prometheus/common/model"
)

const (
	contentType = "application/x-protobuf"
	postPath    = "/api/prom/push"
	getPath     = "/api/prom/label"
)

type entry struct {
	labels model.LabelSet
	logproto.Entry
}

type Loki struct {
	URL       string
	BatchWait time.Duration
	BatchSize int
	quit      chan struct{}
	entry
	wg sync.WaitGroup
}

func (l *Loki) setup() error {
	l.BatchSize = config.Setting.LokiBulk * 1024
	l.BatchWait = time.Duration(config.Setting.LokiTimer) * time.Second
	l.URL = config.Setting.LokiURL
	l.quit = make(chan struct{})

	u, err := url.Parse(l.URL)
	if err != nil {
		return err
	}
	if !strings.Contains(u.Path, postPath) {
		u.Path = postPath
		q := u.Query()
		u.RawQuery = q.Encode()
		l.URL = u.String()
	}
	u.Path = getPath
	q := u.Query()
	u.RawQuery = q.Encode()

	_, err = http.Get(u.String())
	if err != nil {
		return err
	}
	return nil
}

func (l *Loki) send(hCh chan *decoder.HEP) {
	var (
		pkt     *decoder.HEP
		ok      bool
		hepType string
		nodeID  string
	)

	batch := map[model.Fingerprint]*logproto.Stream{}
	batchSize := 0
	//maxWait := time.NewTimer(l.BatchWait)
	maxWait := time.NewTicker(l.BatchWait)

	defer func() {
		if err := l.sendBatch(batch); err != nil {
			logp.Info("heplify-server wants to stop flush remaining loki bulk index requests")
			logp.Err("sendBatch: %v", err)

		}
		l.wg.Done()
	}()

	for {
		select {
		case pkt, ok = <-hCh:
			if !ok {
				break
			}
			nodeID = strconv.Itoa(int(pkt.NodeID))
			hepType = decoder.HEPTypeString(pkt.ProtoType)
			//maxWait.Reset(l.BatchWait)
			switch {
			case pkt.SIP != nil && pkt.ProtoType == 1:
				l.entry = entry{
					model.LabelSet{
						"job":      model.LabelValue("heplify-server"),
						"type":     model.LabelValue(hepType),
						"node_id":  model.LabelValue(nodeID),
						"response": model.LabelValue(pkt.SIP.StartLine.Method),
						"method":   model.LabelValue(pkt.SIP.CseqMethod)},
					logproto.Entry{
						// TODO check Entry out of order errors
						//Timestamp: pkt.Timestamp,
						Timestamp: time.Now(),
						Line:      pkt.Payload,
					}}

			case pkt.ProtoType == 100:
				l.entry = entry{
					model.LabelSet{
						"job":     model.LabelValue("heplify-server"),
						"type":    model.LabelValue(hepType),
						"node_id": model.LabelValue(nodeID)},
					logproto.Entry{
						// TODO check Entry out of order errors
						//Timestamp: pkt.Timestamp,
						Timestamp: time.Now(),
						Line:      pkt.Payload,
					}}
			default:
				continue

			}

			if batchSize+len(l.entry.Line) > l.BatchSize {
				if err := l.sendBatch(batch); err != nil {
					logp.Err("sendBatch: %v", err)
				}
				batchSize = 0
				batch = map[model.Fingerprint]*logproto.Stream{}
			}

			batchSize += len(l.entry.Line)
			fp := l.entry.labels.FastFingerprint()
			stream, ok := batch[fp]
			if !ok {
				stream = &logproto.Stream{
					Labels: l.entry.labels.String(),
				}
				batch[fp] = stream
			}
			stream.Entries = append(stream.Entries, l.Entry)

		case <-maxWait.C:
			if len(batch) > 0 {
				if err := l.sendBatch(batch); err != nil {
					logp.Err("sendBatch: %v", err)
				}
				batchSize = 0
				batch = map[model.Fingerprint]*logproto.Stream{}
			}

		case <-l.quit:
			return

		}
	}
}

func (l *Loki) sendBatch(batch map[model.Fingerprint]*logproto.Stream) error {
	req := logproto.PushRequest{
		Streams: make([]*logproto.Stream, 0, len(batch)),
	}
	count := 0
	for _, stream := range batch {
		req.Streams = append(req.Streams, stream)
		count += len(stream.Entries)
	}
	buf, err := proto.Marshal(&req)
	if err != nil {
		return err
	}
	buf = snappy.Encode(nil, buf)

	resp, err := http.Post(l.URL, contentType, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d - %s", resp.StatusCode, resp.Status)
	}
	logp.Debug("loki", "%s", req)
	return nil
}

// Stop the client.
func (l *Loki) Stop() {
	close(l.quit)
	l.wg.Wait()
}