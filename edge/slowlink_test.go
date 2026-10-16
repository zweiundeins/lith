package edge

import (
	"net/http/httptest"
	"testing"
)

func TestSlowLink(t *testing.T) {
	for _, c := range []struct {
		name    string
		url     string
		headers map[string]string
		want    bool
	}{
		{"nothing said", "/stream?tab=t1", nil, false},
		{"net=slow, from a page of an earlier template", "/stream?tab=t1&net=slow", nil, true},
		{"another value of net", "/stream?tab=t1&net=fast", nil, false},
		{"Save-Data, from the browser or the page", "/stream?tab=t1", map[string]string{"Save-Data": "on"}, true},
		{"Save-Data in capitals", "/stream", map[string]string{"Save-Data": " ON "}, true},
		{"Save-Data off", "/stream", map[string]string{"Save-Data": "off"}, false},
		{"ECT 4g", "/stream", map[string]string{"ECT": "4g", "RTT": "100", "Downlink": "10"}, false},
		{"ECT 3g", "/stream", map[string]string{"ECT": "3g"}, true},
		{"ECT 2g", "/stream", map[string]string{"ECT": "2g"}, true},
		{"ECT slow-2g", "/stream", map[string]string{"ECT": "slow-2g"}, true},
		{"RTT at the threshold", "/stream", map[string]string{"RTT": "300"}, false},
		{"RTT over it", "/stream", map[string]string{"RTT": "325"}, true},
		{"Downlink at the threshold", "/stream", map[string]string{"Downlink": "0.8"}, false},
		{"Downlink under it", "/stream", map[string]string{"Downlink": "0.75"}, true},
		{"Downlink unknown", "/stream", map[string]string{"Downlink": "0"}, false},
		{"hints that do not parse", "/stream", map[string]string{"ECT": "5g", "RTT": "slow", "Downlink": "-"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.url, nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := SlowLink(r); got != c.want {
				t.Errorf("SlowLink = %v, want %v", got, c.want)
			}
		})
	}
}
