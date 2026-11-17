package openrtb2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prebid/prebid-server/v4/analytics"
	analyticsBuild "github.com/prebid/prebid-server/v4/analytics/build"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/exchange"
	"github.com/prebid/prebid-server/v4/hooks"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/ortb"
	"github.com/prebid/prebid-server/v4/privacy"
	"github.com/prebid/prebid-server/v4/stored_requests/backends/empty_fetcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecycleTestRequest = `{
	"id": "req-1",
	"site": {"page": "prebid.org", "publisher": {"id": "a3de7af2"}},
	"imp": [{
		"id": "imp-1",
		"banner": {"format": [{"w": 300, "h": 250}]},
		"ext": {"prebid": {"bidder": {"appnexus": {"placementId": 12883451}, "disabledbidder": {}}}}
	}],
	"ext": {"prebid": {"passthrough": {"sampled": true}}}
}`

type lifecycleRunnerStub struct {
	analytics.Runner
	lifecycle          *analytics.AuctionLifecycle
	gotPassthrough     json.RawMessage
	loggedLifecycle    *analytics.AuctionLifecycle
	loggedAuctionCount int
}

func (r *lifecycleRunnerStub) BeginAuctionLifecycle(passthrough json.RawMessage) *analytics.AuctionLifecycle {
	r.gotPassthrough = passthrough
	return r.lifecycle
}

func (r *lifecycleRunnerStub) LogAuctionObject(ao *analytics.AuctionObject, _ privacy.ActivityControl) {
	r.loggedAuctionCount++
	r.loggedLifecycle = ao.Lifecycle
}

type lifecycleCaptureExchange struct {
	nobidExchange
	lifecycle *analytics.AuctionLifecycle
}

func (e *lifecycleCaptureExchange) HoldAuction(ctx context.Context, r *exchange.AuctionRequest, debugLog *exchange.DebugLog) (*exchange.AuctionResponse, error) {
	e.lifecycle = r.Lifecycle
	return e.nobidExchange.HoldAuction(ctx, r, debugLog)
}

func newLifecycleTestDeps(ex exchange.Exchange, runner analytics.Runner) *endpointDeps {
	disabledBidders := map[string]string{"disabledbidder": "disabledbidder is disabled"}
	return &endpointDeps{
		fakeUUIDGenerator{},
		ex,
		ortb.NewRequestValidator(openrtb_ext.BuildBidderMap(), disabledBidders, mockBidderParamValidator{}),
		&mockStoredReqFetcher{},
		empty_fetcher.EmptyFetcher{},
		empty_fetcher.EmptyFetcher{},
		&config.Configuration{MaxRequestSize: maxSize},
		&metricsConfig.NilMetricsEngine{},
		runner,
		disabledBidders,
		false,
		[]byte{},
		openrtb_ext.BuildBidderMap(),
		nil,
		nil,
		hardcodedResponseIPValidator{response: true},
		empty_fetcher.EmptyFetcher{},
		hooks.EmptyPlanBuilder{},
		nil,
		openrtb_ext.NormalizeBidderName,
	}
}

func sampledLifecycle() *analytics.AuctionLifecycle {
	return analytics.NewAuctionLifecycle(map[string]*analytics.LifecycleOptions{
		"recorder": {},
	})
}

func runLifecycleAuction(t *testing.T, ex exchange.Exchange, runner analytics.Runner) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/openrtb2/auction", strings.NewReader(lifecycleTestRequest))
	newLifecycleTestDeps(ex, runner).Auction(recorder, request, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	return recorder
}

func TestAuctionRecordsLifecycleForSampledAuction(t *testing.T) {
	lifecycle := sampledLifecycle()
	runner := &lifecycleRunnerStub{Runner: analyticsBuild.New(&config.Analytics{}), lifecycle: lifecycle}
	ex := &lifecycleCaptureExchange{}

	runLifecycleAuction(t, ex, runner)

	assert.JSONEq(t, `{"sampled":true}`, string(runner.gotPassthrough))
	assert.Same(t, lifecycle, ex.lifecycle)
	assert.Same(t, lifecycle, runner.loggedLifecycle)
	assert.Equal(t, 1, runner.loggedAuctionCount)
	assert.Equal(t, []analytics.FilteredBidderImpDetail{
		{Bidder: "disabledbidder", ImpID: "imp-1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedGeneral, ReasonDetail: ortb.BidderRemovedDisabled},
	}, lifecycle.FilteredBidderImpDetails)
}

func TestAuctionWithoutLifecycleForUnsampledAuction(t *testing.T) {
	runner := &lifecycleRunnerStub{Runner: analyticsBuild.New(&config.Analytics{})}
	ex := &lifecycleCaptureExchange{}

	runLifecycleAuction(t, ex, runner)

	assert.Nil(t, ex.lifecycle)
	assert.Nil(t, runner.loggedLifecycle)
	assert.Equal(t, 1, runner.loggedAuctionCount)
}

func TestAuctionResponseUnchangedByLifecycle(t *testing.T) {
	plain := runLifecycleAuction(t, &lifecycleCaptureExchange{}, analyticsBuild.New(&config.Analytics{}))
	sampled := runLifecycleAuction(t, &lifecycleCaptureExchange{}, &lifecycleRunnerStub{
		Runner:    analyticsBuild.New(&config.Analytics{}),
		lifecycle: sampledLifecycle(),
	})

	assert.Equal(t, plain.Header(), sampled.Header())
	assert.Equal(t, plain.Body.String(), sampled.Body.String())
}
