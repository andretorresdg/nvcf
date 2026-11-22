// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/webhook"
)

// Single flight (docs/design/model-cache-single-flight.md): per key, one
// pod in the cluster downloads, chosen by a Lease; every other pod of the
// key waits while its node's agent streams the download into its volume,
// from the downloading node or from a node already streaming (a relay).
// The pods learn their part from plan files in the key's directory.

const (
	leasePrefix = "nvsnap-model-dl-"
	// feedsAnnotation, on the Lease, maps each streaming pod's identity to
	// the identity it streams from, so no source feeds more than maxFeeds.
	feedsAnnotation = "nvsnap.io/feeds"
	maxFeeds        = 3
	roleHolder      = "holder"
	roleFollower    = "follower"
	roleNone        = "none" // downloading on its own; left alone
)

func leaseName(key string) string { return leasePrefix + key }

// planned reports whether admission wrapped the pod's init for single
// flight: only such a pod's helper reads plans.
func planned(pod *corev1.Pod, d webhook.ModelCacheDownload) bool {
	for _, a := range pod.Spec.InitContainers[d.InitIndex].Command {
		if a == "--plan-wait" {
			return true
		}
		if a == "--" {
			return false
		}
	}
	return false
}

// flight is one single-flight pod on this node.
type flight struct {
	key, identity string
	uid           types.UID
	role          string
	vol           string
	src           *liveSource // what this node serves for the key, when this pod's volume is it
	cancel        context.CancelFunc
	finalized     bool
}

type leaseObs struct {
	holder, renew string
	at            time.Time
}

// flightPod handles a single-flight pod whose init has not finished. It
// returns false when the pod should take today's path instead (a peer
// has a complete copy, to fetch as today).
func (c *nodeModelCache) flightPod(ctx context.Context, pod *corev1.Pod, d webhook.ModelCacheDownload, st *corev1.ContainerStatus, have bool) bool {
	c.mu.Lock()
	f := c.flights[pod.UID]
	c.mu.Unlock()
	if f != nil {
		if f.role == roleHolder {
			c.holderStep(pod, f)
		}
		return true
	}
	if have || st == nil || st.State.Running == nil {
		// A node with a copy needs no plan: the helper seeds from it. A
		// pod whose init is not running yet takes no Lease: a holder that
		// cannot start would hold everyone.
		return true
	}
	if len(c.sources(ctx, d.Key)) > 0 {
		return false
	}
	vol, err := c.emptyDirPath(pod, d.Landing.VolumeName)
	if err != nil {
		return true
	}
	identity := c.node + "/" + string(pod.UID)
	log := c.log.WithFields(logrus.Fields{"key": d.Key, "pod": pod.Namespace + "/" + pod.Name})
	if c.holds(ctx, d.Key, identity) {
		// This agent restarted while its pod downloads: keep the Lease,
		// or it expires into a second download.
		log.Info("model cache: single flight: this pod still holds the download; keeping it")
		c.becomeHolder(ctx, d.Key, identity, pod.UID, vol, log)
		return true
	}
	if !dirEmpty(vol) {
		// The helper gave up waiting for a plan and downloads.
		c.track(&flight{key: d.Key, identity: identity, uid: pod.UID, role: roleNone, vol: vol})
		return true
	}
	got, holder, err := c.acquire(ctx, d.Key, identity)
	if err != nil {
		log.WithError(err).Warn("model cache: single flight: the Lease is unavailable; the pod downloads on its own")
		return true
	}
	if got {
		c.becomeHolder(ctx, d.Key, identity, pod.UID, vol, log)
		return true
	}
	c.becomeFollower(ctx, d.Key, identity, pod.UID, vol, holder, log)
	return true
}

func (c *nodeModelCache) track(f *flight) {
	c.mu.Lock()
	c.flights[f.uid] = f
	c.mu.Unlock()
}

func dirEmpty(dir string) bool {
	ents, err := os.ReadDir(dir)
	return err == nil && len(ents) == 0
}

func (c *nodeModelCache) becomeHolder(ctx context.Context, key, identity string, uid types.UID, vol string, log logrus.FieldLogger) {
	src := newLiveSource(vol, string(uid), c.timing)
	fctx, cancel := context.WithCancel(ctx)
	f := &flight{key: key, identity: identity, uid: uid, role: roleHolder, vol: vol, src: src, cancel: cancel}
	c.mu.Lock()
	c.liveSources[key] = src
	c.flights[uid] = f
	c.mu.Unlock()
	if err := modelcache.WritePlan(c.store.KeyDir(key), string(uid), modelcache.PlanDownload); err != nil {
		log.WithError(err).Warn("model cache: single flight: writing the plan failed")
	}
	log.Info("model cache: single flight: this pod downloads; others stream from it")
	go c.renewLoop(fctx, f)
}

// holderStep finalizes once the helper signals its download is over.
func (c *nodeModelCache) holderStep(pod *corev1.Pod, f *flight) {
	if f.finalized {
		return
	}
	sig, err := c.emptyDirPath(pod, webhook.ModelCacheSignalVolume)
	if err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(sig, modelcache.SignalFile))
	if err != nil {
		return
	}
	f.finalized = true
	fp := strings.TrimSpace(string(b))
	keyDir := c.store.KeyDir(f.key)
	log := c.log.WithFields(logrus.Fields{"key": f.key, "pod": pod.Namespace + "/" + pod.Name})
	start := time.Now()
	if err := f.src.finalize(fp); err != nil {
		log.WithError(err).Warn("model cache: single flight: no final manifest; streaming pods download")
		// Any value but the tree tells the helper to stop holding.
		_ = writeMarker(keyDir, modelcache.FinalFile(string(f.uid)), "failed")
		return
	}
	_ = writeMarker(keyDir, modelcache.FinalFile(string(f.uid)), fp)
	log.WithField("seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds())).Info("model cache: single flight: final manifest served")
}

func writeMarker(dir, name, content string) error {
	if name == "" {
		return os.ErrInvalid
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(content+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// renewLoop keeps the Lease while the holder downloads, and releases it
// once the node has published the key (others then fetch it as today),
// or the source failed. It stops on losing the Lease.
func (c *nodeModelCache) renewLoop(ctx context.Context, f *flight) {
	t := time.NewTicker(c.leaseDuration / 6)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, _, published := c.store.Latest(f.key)
		if published || f.src.view().State == liveFailed {
			c.release(ctx, f.key, f.identity)
			return
		}
		if err := c.renew(ctx, f.key, f.identity); err != nil {
			c.log.WithError(err).WithField("key", f.key).Warn("model cache: single flight: lost the Lease")
			return
		}
	}
}

// acquire takes the key's Lease for identity if it is free or expired,
// and returns who holds it otherwise. Expired is judged by how long this
// agent has seen the renew time stand still, as client-go's leader
// election does, never by comparing clocks across nodes.
func (c *nodeModelCache) acquire(ctx context.Context, key, identity string) (bool, string, error) {
	leases := c.kube.CoordinationV1().Leases(c.ns)
	dur := int32(c.leaseDuration / time.Second)
	if dur < 1 {
		dur = 1
	}
	now := metav1.NewMicroTime(time.Now())
	l, err := leases.Get(ctx, leaseName(key), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = leases.Create(ctx, &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName(key), Namespace: c.ns,
			Labels: map[string]string{"app.kubernetes.io/component": modelCacheComponent}},
			Spec: coordv1.LeaseSpec{HolderIdentity: &identity, LeaseDurationSeconds: &dur, AcquireTime: &now, RenewTime: &now}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return false, "", nil
		}
		return err == nil, identity, err
	}
	if err != nil {
		return false, "", err
	}
	holder := ""
	if l.Spec.HolderIdentity != nil {
		holder = *l.Spec.HolderIdentity
	}
	if holder == identity {
		return true, holder, nil
	}
	renew := ""
	if l.Spec.RenewTime != nil {
		renew = l.Spec.RenewTime.String()
	}
	c.mu.Lock()
	o, seen := c.leaseSeen[key]
	if !seen || o.holder != holder || o.renew != renew {
		c.leaseSeen[key] = leaseObs{holder: holder, renew: renew, at: time.Now()}
		c.mu.Unlock()
		return false, holder, nil
	}
	c.mu.Unlock()
	if time.Since(o.at) <= c.leaseDuration {
		return false, holder, nil
	}
	l.Spec.HolderIdentity, l.Spec.AcquireTime, l.Spec.RenewTime = &identity, &now, &now
	l.Annotations = nil // the old holder's feeds
	if _, err := leases.Update(ctx, l, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return false, holder, nil
		}
		return false, holder, err
	}
	return true, identity, nil
}

// holds reports whether identity holds the key's Lease.
func (c *nodeModelCache) holds(ctx context.Context, key, identity string) bool {
	l, err := c.kube.CoordinationV1().Leases(c.ns).Get(ctx, leaseName(key), metav1.GetOptions{})
	return err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == identity
}

func (c *nodeModelCache) renew(ctx context.Context, key, identity string) error {
	leases := c.kube.CoordinationV1().Leases(c.ns)
	l, err := leases.Get(ctx, leaseName(key), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != identity {
		return errors.New("held by another")
	}
	now := metav1.NewMicroTime(time.Now())
	l.Spec.RenewTime = &now
	_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
	return err
}

// release deletes the key's Lease if identity still holds it, on that
// version only: a holder back from a partition cannot delete a
// successor's Lease.
func (c *nodeModelCache) release(ctx context.Context, key, identity string) {
	leases := c.kube.CoordinationV1().Leases(c.ns)
	l, err := leases.Get(ctx, leaseName(key), metav1.GetOptions{})
	if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != identity {
		return
	}
	rv := l.ResourceVersion
	_ = leases.Delete(ctx, l.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &rv}})
}

// pickSource chooses what identity streams from: the holder, or a pod
// already streaming, whichever feeds the fewest (at most maxFeeds), and
// records the choice on the Lease. With no room anywhere, the holder.
func (c *nodeModelCache) pickSource(ctx context.Context, key, identity, holder string) string {
	leases := c.kube.CoordinationV1().Leases(c.ns)
	for attempt := 0; attempt < 5; attempt++ {
		l, err := leases.Get(ctx, leaseName(key), metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != holder {
			return holder
		}
		feeds := map[string]string{}
		if s := l.Annotations[feedsAnnotation]; s != "" {
			_ = json.Unmarshal([]byte(s), &feeds)
		}
		delete(feeds, identity)
		count := map[string]int{holder: 0}
		for consumer, src := range feeds {
			count[src]++
			if _, ok := count[consumer]; !ok {
				count[consumer] = 0
			}
		}
		var cands []string
		for id := range count {
			if id != identity {
				cands = append(cands, id)
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			ci, cj := count[cands[i]], count[cands[j]]
			if ci != cj {
				return ci < cj
			}
			if (cands[i] == holder) != (cands[j] == holder) {
				return cands[i] == holder
			}
			return cands[i] < cands[j]
		})
		pick := holder
		if len(cands) > 0 && count[cands[0]] < maxFeeds {
			pick = cands[0]
		}
		feeds[identity] = pick
		b, _ := json.Marshal(feeds)
		if l.Annotations == nil {
			l.Annotations = map[string]string{}
		}
		l.Annotations[feedsAnnotation] = string(b)
		if _, err := leases.Update(ctx, l, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		}
		return pick
	}
	return holder
}

func (c *nodeModelCache) clientFor(key, source string) liveClient {
	node := source
	if i := strings.IndexByte(source, '/'); i >= 0 {
		node = source[:i]
	}
	if node == c.node {
		c.mu.Lock()
		src := c.liveSources[key]
		c.mu.Unlock()
		if src != nil {
			return localClient{src}
		}
	}
	return &httpLiveClient{c: c, node: node, key: key}
}

func (c *nodeModelCache) becomeFollower(ctx context.Context, key, identity string, uid types.UID, vol, holder string, log logrus.FieldLogger) {
	keyDir := c.store.KeyDir(key)
	stopHB := c.heartbeatFile(key, modelcache.StreamingFile)
	if err := modelcache.WritePlan(keyDir, string(uid), modelcache.PlanWait); err != nil {
		stopHB()
		log.WithError(err).Warn("model cache: single flight: writing the plan failed")
		return
	}
	fctx, cancel := context.WithCancel(ctx)
	f := &flight{key: key, identity: identity, uid: uid, role: roleFollower, vol: vol, cancel: cancel}
	c.mu.Lock()
	if c.liveSources[key] == nil {
		// This pod's volume is a relay for later streaming pods.
		f.src = newLiveSource(vol, string(uid), c.timing)
		c.liveSources[key] = f.src
	}
	c.flights[uid] = f
	c.mu.Unlock()
	go c.follow(fctx, f, holder, stopHB, log)
}

// follow streams into the pod's volume until it is done, then marks it.
// When the source fails it takes the Lease if it can (the pod downloads,
// into its emptied volume), else streams from the new holder, keeping
// what it copied: only the final manifest decides what stays.
func (c *nodeModelCache) follow(ctx context.Context, f *flight, holder string, stopHB func(), log logrus.FieldLogger) {
	defer stopHB()
	keyDir := c.store.KeyDir(f.key)
	s := &streamer{dst: f.vol, timing: c.timing}
	for {
		source := c.pickSource(ctx, f.key, f.identity, holder)
		s.client = c.clientFor(f.key, source)
		log.WithField("source", source).Info("model cache: single flight: streaming from another node")
		start := time.Now()
		err := s.run(ctx)
		if err == nil {
			if f.src != nil {
				f.src.setDone(*s.final)
			}
			_ = writeMarker(keyDir, modelcache.DoneFile(string(f.uid)), "")
			log.WithField("seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds())).Info("model cache: single flight: streamed; the pod starts")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errStall) {
			log.Warn("model cache: single flight: the source made no progress; the pod downloads")
			return
		}
		got, h, aerr := c.acquire(ctx, f.key, f.identity)
		if aerr != nil {
			log.WithError(aerr).Warn("model cache: single flight: the Lease is unavailable; the pod downloads")
			return
		}
		if got {
			// Stop writing, empty the volume, and only then tell the
			// helper to download into it.
			if err := emptyVolume(f.vol); err != nil {
				log.WithError(err).Warn("model cache: single flight: emptying the volume failed; the pod downloads")
				return
			}
			c.mu.Lock()
			if c.liveSources[f.key] == f.src {
				delete(c.liveSources, f.key)
			}
			c.mu.Unlock()
			c.becomeHolder(context.WithoutCancel(ctx), f.key, f.identity, f.uid, f.vol, log)
			return
		}
		if h == "" {
			h = holder
		}
		holder = h
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.timing.poll):
		}
	}
}

func emptyVolume(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// endFlights stops the flights of pods no longer on the node: a holder's
// source fails (its streaming pods move on) and its Lease is released.
func (c *nodeModelCache) endFlights(ctx context.Context, live map[types.UID]bool) {
	c.mu.Lock()
	var gone []*flight
	for uid, f := range c.flights {
		if !live[uid] {
			gone = append(gone, f)
			delete(c.flights, uid)
		}
	}
	c.mu.Unlock()
	for _, f := range gone {
		if f.cancel != nil {
			f.cancel()
		}
		if f.src != nil {
			f.src.fail()
			c.mu.Lock()
			if c.liveSources[f.key] == f.src {
				delete(c.liveSources, f.key)
			}
			c.mu.Unlock()
		}
		if f.role == roleHolder {
			c.release(ctx, f.key, f.identity)
		}
		keyDir := c.store.KeyDir(f.key)
		modelcache.RemovePlan(keyDir, string(f.uid))
		for _, name := range []string{modelcache.FinalFile(string(f.uid)), modelcache.DoneFile(string(f.uid))} {
			if name != "" {
				_ = os.Remove(filepath.Join(keyDir, name))
			}
		}
	}
}

// Live routes. Behind the agent's token guard like every agent-to-agent
// route, under a prefix the cache's own routes do not match.

func (c *nodeModelCache) addLiveRoutes(r *mux.Router) {
	r.HandleFunc("/v1/model-cache-live/{key}", c.liveViewHandler).Methods("GET")
	r.HandleFunc("/v1/model-cache-live/{key}/file", c.liveFileHandler).Methods("GET")
}

func (c *nodeModelCache) liveSourceFor(w http.ResponseWriter, r *http.Request) *liveSource {
	key := mux.Vars(r)["key"]
	if !modelcache.ValidKey(key) {
		http.Error(w, "bad key", http.StatusBadRequest)
		return nil
	}
	c.mu.Lock()
	src := c.liveSources[key]
	c.mu.Unlock()
	if src == nil {
		http.NotFound(w, r)
	}
	return src
}

func (c *nodeModelCache) liveViewHandler(w http.ResponseWriter, r *http.Request) {
	if src := c.liveSourceFor(w, r); src != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(src.view())
	}
}

func (c *nodeModelCache) liveFileHandler(w http.ResponseWriter, r *http.Request) {
	src := c.liveSourceFor(w, r)
	if src == nil {
		return
	}
	f, err := src.open(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

// httpLiveClient reads a source on another node.
type httpLiveClient struct {
	c         *nodeModelCache
	node, key string
	fails     int
}

func (h *httpLiveClient) base(ctx context.Context) (string, error) {
	b, err := h.c.peerURL(ctx, h.node)
	return b + "/v1/model-cache-live/" + h.key, err
}

func (h *httpLiveClient) View(ctx context.Context) (liveView, error) {
	var v liveView
	b, err := h.base(ctx)
	if err == nil {
		err = h.c.getJSON(ctx, b, &v)
	}
	if err != nil {
		// A source that is gone is a failed one, after a few polls.
		if h.fails++; h.fails >= 5 {
			return liveView{State: liveFailed}, nil
		}
		return v, err
	}
	h.fails = 0
	return v, nil
}

func (h *httpLiveClient) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	b, err := h.base(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b+"/file?path="+url.QueryEscape(rel), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("source answered %s for %s", resp.Status, rel)
	}
	return resp.Body, nil
}
