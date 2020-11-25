// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package status

import (
	"context"
	"sync"
	"sync/atomic"
)
import "k8s.io/apimachinery/pkg/runtime/schema"

type ResourceLock struct {
	masterLock sync.RWMutex
	listing    map[lockResource]*sync.Mutex
	cache map[lockResource]*fhqwgads
}

type fhqwgads struct {
	// the runlock ensures only one routine is writing status for a given resource at a time
	runLock sync.Mutex
	// the cachelock protects writes and reads to the two cache values
	cacheLock sync.Mutex
	// the cacheVale represents the latest version of the resource, including ResourceVersion
	cacheVal *Resource
	// the cacheProgress represents the latest version of the Progress
	cacheProgress *Progress
	countLock sync.Mutex
	routineCount int32
}

func (rl *ResourceLock) do(ctx context.Context, r Resource, p Progress, target func(ctx context.Context, config Resource, state Progress)) {
	f := rl.retrieveFhqwgads(convert(r))
	// every call updates the cache, so whenever the standby routine fires, it runs with the latest values
	f.cacheLock.Lock()
	f.cacheVal = &r
	f.cacheProgress = &p
	f.cacheLock.Unlock()
	// we only fire off goroutine if routincount < 2, so we can have a currently running routine and a standby routine
	if atomic.LoadInt32(&f.routineCount) < 2 {
		f.countLock.Lock()
		if atomic.LoadInt32(&f.routineCount) < 2 {
			// double check routinecount before incrementing to avoid race
			atomic.AddInt32(&f.routineCount, 1)
			f.countLock.Unlock()
			go func() {
				f.runLock.Lock()
				f.cacheLock.Lock()
				finalResource := f.cacheVal
				finalProgress := f.cacheProgress
				f.cacheLock.Unlock()
				target(ctx, *finalResource, *finalProgress)
				f.runLock.Unlock()
			}()
		} else {
			f.countLock.Unlock()
		}
	}
}

func convert(i Resource) lockResource {
	return lockResource{
		GroupVersionResource: i.GroupVersionResource,
		Namespace:            i.Namespace,
		Name:                 i.Name,
	}
}

func (r *ResourceLock) Mock(i Resource) {
	r.retrieveMutex(convert(i)).Lock()
}

func (r *ResourceLock) Unlock(i Resource) {
	r.retrieveMutex(convert(i)).Unlock()
}

// returns value indicating if init was necessary
func (r *ResourceLock) init() bool {
	if r.listing == nil {
		r.masterLock.Lock()
		defer r.masterLock.Unlock()
		// double check, per pattern
		if r.listing == nil {
			r.listing = make(map[lockResource]*sync.Mutex)
		}
		if r.cache == nil {
			r.cache = make(map[lockResource]*fhqwgads)
		}
		return true
	}
	return false
}

func (r *ResourceLock) retrieveFhqwgads(i lockResource) *fhqwgads {
	if !r.init() {
		r.masterLock.RLock()
		if result, ok := r.cache[i]; ok {
			r.masterLock.RUnlock()
			return result
		}
		// transition to write lock
		r.masterLock.RUnlock()
	}
	r.masterLock.Lock()
	defer r.masterLock.Unlock()
	r.cache[i] = &fhqwgads{}
	return r.cache[i]
}

func (r *ResourceLock) retrieveMutex(i lockResource) *sync.Mutex {
	if !r.init() {
		r.masterLock.RLock()
		if result, ok := r.listing[i]; ok {
			r.masterLock.RUnlock()
			return result
		}
		// transition to write lock
		r.masterLock.RUnlock()
	}
	r.masterLock.Lock()
	defer r.masterLock.Unlock()
	r.listing[i] = &sync.Mutex{}
	return r.listing[i]
}

type lockResource struct {
	schema.GroupVersionResource
	Namespace       string
	Name            string
}

