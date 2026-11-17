package build

import (
	"encoding/json"
	"testing"

	"github.com/prebid/prebid-server/v4/analytics"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/privacy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type lifecycleModule struct {
	sampleModule
	record    func(json.RawMessage) *analytics.LifecycleOptions
	completed []*analytics.AuctionLifecycle
}

func newLifecycleModule(record func(json.RawMessage) *analytics.LifecycleOptions) *lifecycleModule {
	return &lifecycleModule{
		sampleModule: sampleModule{count: new(int)},
		record:       record,
	}
}

func recordEveryAuction(bodyBidders ...string) func(json.RawMessage) *analytics.LifecycleOptions {
	return func(json.RawMessage) *analytics.LifecycleOptions {
		return &analytics.LifecycleOptions{RequestBodyBidders: bodyBidders}
	}
}

func (m *lifecycleModule) AuctionLifecycleOptions(passthrough json.RawMessage) *analytics.LifecycleOptions {
	if m.record == nil {
		return nil
	}
	return m.record(passthrough)
}

func (m *lifecycleModule) LogAuctionObject(ao *analytics.AuctionObject) {
	*m.count++
	if ao.Lifecycle != nil {
		m.completed = append(m.completed, ao.Lifecycle)
	}
}

func lifecycleAuctionObject(lifecycle *analytics.AuctionLifecycle) *analytics.AuctionObject {
	return &analytics.AuctionObject{
		RequestWrapper: &openrtb_ext.RequestWrapper{BidRequest: getDefaultBidRequest()},
		Lifecycle:      lifecycle,
	}
}

func TestBeginAuctionLifecycle(t *testing.T) {
	sampled := newLifecycleModule(recordEveryAuction("appnexus"))
	skipped := newLifecycleModule(func(json.RawMessage) *analytics.LifecycleOptions { return nil })
	plain := &sampleModule{count: new(int)}
	runner := enabledAnalytics{
		"sampled": sampled,
		"skipped": skipped,
		"plain":   plain,
	}

	lifecycle := runner.BeginAuctionLifecycle(json.RawMessage(`{}`))

	require.NotNil(t, lifecycle)
	assert.True(t, lifecycle.KeepsRequestBody("appnexus"))
	assert.False(t, lifecycle.KeepsRequestBody("rubicon"))
}

func TestBeginAuctionLifecycleWhenNoModuleRecords(t *testing.T) {
	runner := enabledAnalytics{
		"module": newLifecycleModule(func(json.RawMessage) *analytics.LifecycleOptions { return nil }),
	}

	assert.Nil(t, runner.BeginAuctionLifecycle(nil))
}

func TestLogAuctionObjectDeliversLifecycleOnlyToOptedInModule(t *testing.T) {
	recording := newLifecycleModule(recordEveryAuction())
	skipped := newLifecycleModule(nil)
	plain := &sampleModule{count: new(int)}
	runner := enabledAnalytics{
		"recording": recording,
		"skipped":   skipped,
		"plain":     plain,
	}
	lifecycle := runner.BeginAuctionLifecycle(nil)
	ao := lifecycleAuctionObject(lifecycle)

	runner.LogAuctionObject(ao, privacy.ActivityControl{})

	assert.Equal(t, []*analytics.AuctionLifecycle{lifecycle}, recording.completed)
	assert.Equal(t, 1, *recording.count)
	assert.Empty(t, skipped.completed)
	assert.Equal(t, 1, *skipped.count)
	assert.Equal(t, 1, *plain.count)
	assert.Same(t, lifecycle, ao.Lifecycle, "runner must restore ao.Lifecycle after the loop")
}

func TestLogAuctionObjectForUnsampledAuction(t *testing.T) {
	recording := newLifecycleModule(recordEveryAuction())
	runner := enabledAnalytics{"recording": recording}

	runner.LogAuctionObject(lifecycleAuctionObject(nil), privacy.ActivityControl{})

	assert.Empty(t, recording.completed)
	assert.Equal(t, 1, *recording.count)
}

func TestLogAuctionObjectAppliesAnalyticsActivitiesToLifecycle(t *testing.T) {
	tests := []struct {
		name             string
		reportAnalytics  bool
		transmitUserFPD  bool
		expectDelivered  bool
		expectWithheld   bool
		expectBodyLength int
	}{
		{name: "allowed", reportAnalytics: true, transmitUserFPD: true, expectDelivered: true, expectBodyLength: 2},
		{name: "user_fpd_blocked", reportAnalytics: true, transmitUserFPD: false, expectDelivered: true, expectWithheld: true},
		{name: "report_analytics_blocked", reportAnalytics: false, transmitUserFPD: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recording := newLifecycleModule(recordEveryAuction("appnexus"))
			runner := enabledAnalytics{"recording": recording}
			lifecycle := runner.BeginAuctionLifecycle(nil)
			lifecycle.BidderRequestDetails = []analytics.BidderRequestDetail{{BidderRequestID: "appnexus-0", Bidder: "appnexus", RequestBody: []byte(`{}`)}}
			ac := privacy.NewActivityControl(getActivityConfig("recording", test.reportAnalytics, test.transmitUserFPD, true))

			runner.LogAuctionObject(lifecycleAuctionObject(lifecycle), ac)

			if !test.expectDelivered {
				assert.Empty(t, recording.completed)
				assert.Equal(t, 0, *recording.count)
				return
			}
			require.Len(t, recording.completed, 1)
			assert.Equal(t, test.expectWithheld, recording.completed[0].RequestBodiesWithheld)
			assert.Len(t, recording.completed[0].BidderRequestDetails[0].RequestBody, test.expectBodyLength)
		})
	}
}
