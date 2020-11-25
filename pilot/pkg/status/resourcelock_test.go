package status

import (
	"context"
	"fmt"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"testing"
	"time"
)

func TestResourceLock_Lock(t *testing.T) {
	g := NewGomegaWithT(t)
	r1 := Resource{
		GroupVersionResource: schema.GroupVersionResource{
			Group: "r1",
			Version: "r1",
		},
		Namespace:            "r1",
		Name:                 "r1",
		ResourceVersion:      "r1",
	}
	r2 := Resource{
		GroupVersionResource: schema.GroupVersionResource{
			Group: "r2",
			Version: "r2",
		},
		Namespace:            "r2",
		Name:                 "r2",
		ResourceVersion:      "r2",
	}
	r1a := Resource{
		GroupVersionResource: schema.GroupVersionResource{
			Group: "r1",
			Version: "r1",
		},
		Namespace:            "r1",
		Name:                 "r1",
		ResourceVersion:      "r1",
	}
	rlock := ResourceLock{}
	fmt.Printf("fkdkd\n")
	rlock.do(nil, r1, Progress{}, func(_ context.Context, config Resource, _ Progress){
		fmt.Printf("starting first %s\n", config.Name)
		time.Sleep(4*time.Second)
		fmt.Printf("finishing first %s\n", config.Name)
	} )
	rlock.do(nil, r1a, Progress{}, func(_ context.Context, config Resource, _ Progress){
		fmt.Printf("starting second %s\n", config.Name)
		time.Sleep(4*time.Second)
		fmt.Printf("finishing second %s\n", config.Name)
	} )
	for i := 0; i <1000; i++ {
		rlock.do(nil, r1, Progress{}, func(_ context.Context, config Resource, _ Progress){
			fmt.Printf("starting third %s\n", config.Name)
			time.Sleep(4*time.Second)
			fmt.Printf("finishing third %s\n", config.Name)
		} )
	}
	time.Sleep(12*time.Second)
	fmt.Printf("map len %d", len(rlock.cache))
	rlock.Mock(r1)
	rlock.Mock(r2)
	rlock.Unlock(r2)
	rlock.Unlock(r1)
	rlock.Mock(r1)
	rlock.Unlock(r1)
	r1.ResourceVersion = "2"
	rlock.Mock(r1)
	rlock.Unlock(r1)
	rlock.Mock(r1a)
	rlock.Unlock(r1a)
	g.Expect(rlock.listing).To(HaveLen(2))

}
