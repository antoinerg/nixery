// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.
package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"sync"

	"github.com/google/nixery/server/manifest"
	log "github.com/sirupsen/logrus"
)

// LocalCache implements the structure used for local caching of
// manifests and layer uploads.
type LocalCache struct {
	// Manifest cache
	mmtx sync.RWMutex
	mdir string

	// Layer cache
	lmtx   sync.RWMutex
	lcache map[string]manifest.Entry
}

// Creates an in-memory cache and ensures that the local file path for
// manifest caching exists.
func NewCache() (LocalCache, error) {
	path := os.TempDir() + "/nixery"
	err := os.MkdirAll(path, 0755)
	if err != nil {
		return LocalCache{}, err
	}

	return LocalCache{
		mdir:   path + "/",
		lcache: make(map[string]manifest.Entry),
	}, nil
}

// Retrieve a cached manifest if the build is cacheable and it exists.
func (c *LocalCache) manifestFromLocalCache(key string) (json.RawMessage, bool) {
	c.mmtx.RLock()
	defer c.mmtx.RUnlock()

	f, err := os.Open(c.mdir + key)
	if err != nil {
		// This is a debug log statement because failure to
		// read the manifest key is currently expected if it
		// is not cached.
		log.WithError(err).WithField("manifest", key).
			Debug("failed to read manifest from local cache")

		return nil, false
	}
	defer f.Close()

	m, err := ioutil.ReadAll(f)
	if err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to read manifest from local cache")

		return nil, false
	}

	return json.RawMessage(m), true
}

// Adds the result of a manifest build to the local cache, if the
// manifest is considered cacheable.
//
// Manifests can be quite large and are cached on disk instead of in
// memory.
func (c *LocalCache) localCacheManifest(key string, m json.RawMessage) {
	c.mmtx.Lock()
	defer c.mmtx.Unlock()

	err := ioutil.WriteFile(c.mdir+key, []byte(m), 0644)
	if err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to locally cache manifest")
	}
}

// Retrieve a layer build from the local cache.
func (c *LocalCache) layerFromLocalCache(key string) (*manifest.Entry, bool) {
	c.lmtx.RLock()
	e, ok := c.lcache[key]
	c.lmtx.RUnlock()

	return &e, ok
}

// Add a layer build result to the local cache.
func (c *LocalCache) localCacheLayer(key string, e manifest.Entry) {
	c.lmtx.Lock()
	c.lcache[key] = e
	c.lmtx.Unlock()
}

// Retrieve a manifest from the cache(s). First the local cache is
// checked, then the GCS-bucket cache.
func manifestFromCache(ctx context.Context, s *State, key string) (json.RawMessage, bool) {
	if m, cached := s.Cache.manifestFromLocalCache(key); cached {
		return m, true
	}

	obj := s.Bucket.Object("manifests/" + key)

	// Probe whether the file exists before trying to fetch it.
	_, err := obj.Attrs(ctx)
	if err != nil {
		return nil, false
	}

	r, err := obj.NewReader(ctx)
	if err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to retrieve manifest from bucket cache")

		return nil, false
	}
	defer r.Close()

	m, err := ioutil.ReadAll(r)
	if err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to read cached manifest from bucket")

		return nil, false
	}

	go s.Cache.localCacheManifest(key, m)
	log.WithField("manifest", key).Info("retrieved manifest from GCS")

	return json.RawMessage(m), true
}

// Add a manifest to the bucket & local caches
func cacheManifest(ctx context.Context, s *State, key string, m json.RawMessage) {
	go s.Cache.localCacheManifest(key, m)

	obj := s.Bucket.Object("manifests/" + key)
	w := obj.NewWriter(ctx)
	r := bytes.NewReader([]byte(m))

	size, err := io.Copy(w, r)
	if err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to cache manifest to GCS")

		return
	}

	if err = w.Close(); err != nil {
		log.WithError(err).WithField("manifest", key).
			Error("failed to cache manifest to GCS")

		return
	}

	log.WithFields(log.Fields{
		"manifest": key,
		"size":     size,
	}).Info("cached manifest to GCS")
}

// Retrieve a layer build from the cache, first checking the local
// cache followed by the bucket cache.
func layerFromCache(ctx context.Context, s *State, key string) (*manifest.Entry, bool) {
	if entry, cached := s.Cache.layerFromLocalCache(key); cached {
		return entry, true
	}

	obj := s.Bucket.Object("builds/" + key)
	_, err := obj.Attrs(ctx)
	if err != nil {
		return nil, false
	}

	r, err := obj.NewReader(ctx)
	if err != nil {
		log.WithError(err).WithField("layer", key).
			Error("failed to retrieve cached layer from GCS")

		return nil, false
	}
	defer r.Close()

	jb := bytes.NewBuffer([]byte{})
	_, err = io.Copy(jb, r)
	if err != nil {
		log.WithError(err).WithField("layer", key).
			Error("failed to read cached layer from GCS")

		return nil, false
	}

	var entry manifest.Entry
	err = json.Unmarshal(jb.Bytes(), &entry)
	if err != nil {
		log.WithError(err).WithField("layer", key).
			Error("failed to unmarshal cached layer")

		return nil, false
	}

	go s.Cache.localCacheLayer(key, entry)
	return &entry, true
}

func cacheLayer(ctx context.Context, s *State, key string, entry manifest.Entry) {
	s.Cache.localCacheLayer(key, entry)

	obj := s.Bucket.Object("builds/" + key)

	j, _ := json.Marshal(&entry)

	w := obj.NewWriter(ctx)

	_, err := io.Copy(w, bytes.NewReader(j))
	if err != nil {
		log.WithError(err).WithField("layer", key).
			Error("failed to cache layer")

		return
	}

	if err = w.Close(); err != nil {
		log.WithError(err).WithField("layer", key).
			Error("failed to cache layer")

		return
	}
}
