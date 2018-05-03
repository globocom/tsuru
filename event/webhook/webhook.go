// Copyright 2018 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/globalsign/mgo/bson"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tsuru/tsuru/api/shutdown"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/log"
	tsuruNet "github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/storage"
	eventTypes "github.com/tsuru/tsuru/types/event"
	"github.com/tsuru/tsuru/validation"
)

var (
	_ eventTypes.WebHookService = &webHookService{}

	chanBufferSize   = 1000
	defaultUserAgent = "tsuru-webhook-client/1.0"

	webhooksLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "tsuru_webhooks_latency_seconds",
		Help: "The latency for webhooks requests in seconds",
	})

	webhooksQueue = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tsuru_webhooks_event_queue_current",
		Help: "The current number of queued events waiting for webhooks processing",
	})

	webhooksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsuru_webhooks_calls_total",
		Help: "The total number of webhooks calls",
	})

	webhooksError = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsuru_webhooks_calls_error",
		Help: "The total number of webhooks calls with error",
	})
)

func WebHookService() (eventTypes.WebHookService, error) {
	dbDriver, err := storage.GetCurrentDbDriver()
	if err != nil {
		dbDriver, err = storage.GetDefaultDbDriver()
		if err != nil {
			return nil, err
		}
	}
	s := &webHookService{
		storage: dbDriver.WebHookStorage,
		evtCh:   make(chan string, chanBufferSize),
		quitCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go s.run()
	shutdown.Register(s)
	return s, nil
}

type webHookService struct {
	storage eventTypes.WebHookStorage
	evtCh   chan string
	quitCh  chan struct{}
	doneCh  chan struct{}
}

func (s *webHookService) Shutdown(ctx context.Context) error {
	doneCtx := ctx.Done()
	close(s.quitCh)
	select {
	case <-s.doneCh:
	case <-doneCtx:
		return ctx.Err()
	}
	return nil
}

func (s *webHookService) Notify(evtID string) {
	select {
	case s.evtCh <- evtID:
	case <-s.quitCh:
	}
	webhooksQueue.Set(float64(len(s.evtCh)))
}

func (s *webHookService) run() {
	defer close(s.doneCh)
	for {
		select {
		case evtID := <-s.evtCh:
			webhooksQueue.Set(float64(len(s.evtCh)))
			err := s.handleEvent(evtID)
			if err != nil {
				log.Errorf("[webhooks] error handling webhooks for event %s", evtID)
			}
		case <-s.quitCh:
			return
		}
	}
}

func (s *webHookService) handleEvent(evtID string) error {
	evt, err := event.GetByID(bson.ObjectId(evtID))
	if err != nil {
		return err
	}
	filter := eventTypes.WebHookEventFilter{
		TargetTypes:  []string{string(evt.Target.Type)},
		TargetValues: []string{evt.Target.Value},
		KindTypes:    []string{string(evt.Kind.Type)},
		KindNames:    []string{evt.Kind.Name},
	}
	for _, t := range evt.ExtraTargets {
		filter.TargetTypes = append(filter.TargetTypes, string(t.Target.Type))
		filter.TargetValues = append(filter.TargetValues, t.Target.Value)
	}
	hooks, err := s.storage.FindByEvent(filter, evt.Error == "")
	if err != nil {
		return err
	}
	for _, h := range hooks {
		err = s.doHook(h, evt)
		if err != nil {
			log.Errorf("[webhooks] error calling webhook %q: %v", h.Name, err)
		}
	}
	return nil
}

func webhookBody(hook *eventTypes.WebHook, evt *event.Event) (io.Reader, error) {
	if hook.Body != "" {
		return strings.NewReader(hook.Body), nil
	}
	if hook.Method != http.MethodPost &&
		hook.Method != http.MethodPut &&
		hook.Method != http.MethodPatch {
		return nil, nil
	}
	hook.Headers.Set("Content-Type", "application/json")
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (s *webHookService) doHook(hook eventTypes.WebHook, evt *event.Event) (err error) {
	defer func() {
		webhooksTotal.Inc()
		if err != nil {
			webhooksError.Inc()
		}
	}()
	hook.Method = strings.ToUpper(hook.Method)
	if hook.Method == "" {
		hook.Method = http.MethodPost
	}
	body, err := webhookBody(&hook, evt)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(hook.Method, hook.URL, body)
	if err != nil {
		return err
	}
	req.Header = hook.Headers
	if req.UserAgent() == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	client := tsuruNet.Dial5Full60ClientNoKeepAlive
	if hook.Insecure {
		client = &tsuruNet.Dial5Full60ClientNoKeepAliveInsecure
	}
	reqStart := time.Now()
	rsp, err := client.Do(req)
	webhooksLatency.Observe(time.Since(reqStart).Seconds())
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode < 200 || rsp.StatusCode >= 400 {
		data, _ := ioutil.ReadAll(rsp.Body)
		return errors.Errorf("invalid status code calling hook: %d: %s", rsp.StatusCode, string(data))
	}
	return nil
}

func validateURL(u string) error {
	if u == "" {
		return &tsuruErrors.ValidationError{Message: "webhook url must not be empty"}
	}
	_, err := url.Parse(u)
	if err != nil {
		return &tsuruErrors.ValidationError{
			Message: fmt.Sprintf("webhook url is not valid: %v", err),
		}
	}
	return nil
}

func (s *webHookService) Create(w eventTypes.WebHook) error {
	if w.Name == "" {
		return &tsuruErrors.ValidationError{Message: "webhook name must not be empty"}
	}
	err := validation.EnsureValidateName(w.Name)
	if err != nil {
		return err
	}
	err = validateURL(w.URL)
	if err != nil {
		return err
	}
	return s.storage.Insert(w)
}

func (s *webHookService) Update(w eventTypes.WebHook) error {
	err := validateURL(w.URL)
	if err != nil {
		return err
	}
	return s.storage.Update(w)
}

func (s *webHookService) Delete(name string) error {
	return s.storage.Delete(name)
}

func (s *webHookService) Find(name string) (eventTypes.WebHook, error) {
	w, err := s.storage.FindByName(name)
	if err != nil {
		return eventTypes.WebHook{}, err
	}
	return *w, nil
}

func (s *webHookService) List(teams []string) ([]eventTypes.WebHook, error) {
	return s.storage.FindAllByTeams(teams)
}