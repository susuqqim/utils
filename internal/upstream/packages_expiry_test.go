package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestCreditPackagesExpiryTimestamp(t *testing.T) {
	end := time.Now().In(softRateResetLoc).Add(7 * 24 * time.Hour).Truncate(time.Second)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+
			`{"PackageName":"gift","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20,"CycleEndTime":"`+
			end.Format(packageEndLayout)+`"},`+
			`{"PackageName":"unknown","CycleCapacitySize":10,"CycleCapacityRemain":10,"CycleCapacityUsed":0}`+
			`]}}}}`), nil
	})
	packs, remain, size, err := c.CreditPackages(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if remain != 90 || size != 110 {
		t.Fatalf("remain/size=%d/%d want 90/110", remain, size)
	}
	var found bool
	for _, p := range packs {
		if p.Name == "gift" {
			found = p.ExpiresAt == end.UnixMilli()
		}
	}
	if !found {
		t.Fatalf("gift pack missing Unix-ms expiry: %+v", packs)
	}
}
