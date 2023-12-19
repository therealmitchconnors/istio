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

package krt

import (
	"testing"
)

func TestNewPuttableCollection(t *testing.T) {
	out := NewPuttableCollection[string]()
	var x Collection[string] = out
	y := x.(internalCollection[string])
	if y == nil {
		t.Errorf("NewPuttableCollection[string] failed")
	}
}

type itf interface {
	foobar() string
}

type inner struct{}

func (i *inner) foobar() string {
	return "barfoo"
}

type outer struct {
	inner
}

var _ itf = &outer{}

func TestOther(t *testing.T) {
	var x itf = &outer{
		inner: inner{},
	}
	i := x.(*inner)
	if i == nil {
		t.Errorf("failed")
	}
}
