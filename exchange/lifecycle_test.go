package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/analytics"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/currency"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/experiment/adscert"
	"github.com/prebid/prebid-server/v4/gdpr"
	"github.com/prebid/prebid-server/v4/hooks"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/metrics"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/privacy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func recordedLifecycle(bodyBidders ...string) *analytics.AuctionLifecycle {
	return analytics.NewAuctionLifecycle(map[string]*analytics.LifecycleOptions{
		"recorder": {RequestBodyBidders: bodyBidders},
	})
}

type excludeBidderHook struct{ bidder, reason string }

func (h excludeBidderHook) HandleProcessedAuctionHook(
	_ context.Context,
	_ hookstage.ModuleInvocationContext,
	_ hookstage.ProcessedAuctionRequestPayload,
) (hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload], error) {
	result := hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload]{}
	result.ChangeSet.ProcessedAuctionRequest().Bidders().DeleteWithReason(map[string]struct{}{h.bidder: {}}, h.reason)
	return result, nil
}

type excludeBidderPlanBuilder struct {
	hooks.EmptyPlanBuilder
	hook excludeBidderHook
}

func (b excludeBidderPlanBuilder) PlanForProcessedAuctionStage(_ string, _ *config.Account) hooks.Plan[hookstage.ProcessedAuctionRequest] {
	return hooks.Plan[hookstage.ProcessedAuctionRequest]{{
		Timeout: time.Second,
		Hooks:   []hooks.HookWrapper[hookstage.ProcessedAuctionRequest]{{Module: "rules", Code: "exclude", Hook: b.hook}},
	}}
}

func requestWithBidders(t *testing.T, impIDs []string, bidders ...string) *openrtb_ext.RequestWrapper {
	t.Helper()
	params := make(map[string]json.RawMessage, len(bidders))
	for _, bidder := range bidders {
		params[bidder] = json.RawMessage(`{}`)
	}
	ext, err := json.Marshal(map[string]any{"prebid": map[string]any{"bidder": params}})
	require.NoError(t, err)

	request := &openrtb2.BidRequest{ID: "auction"}
	for _, impID := range impIDs {
		request.Imp = append(request.Imp, openrtb2.Imp{ID: impID, Ext: ext})
	}
	return &openrtb_ext.RequestWrapper{BidRequest: request}
}

func TestProcessedAuctionCaptureRecordsHookRemovals(t *testing.T) {
	lifecycle := recordedLifecycle()
	executor := hookexecution.NewHookExecutor(
		excludeBidderPlanBuilder{hook: excludeBidderHook{bidder: "bidderB", reason: "drop-low-perf"}},
		hookexecution.EndpointAuction,
		&metricsConfig.NilMetricsEngine{},
	)
	request := requestWithBidders(t, []string{"imp1", "imp2"}, "bidderA", "bidderB")

	startProcessedAuctionCapture(lifecycle, executor)
	require.NoError(t, executor.ExecuteProcessedAuctionStage(request))

	assert.Equal(t, []analytics.FilteredBidderImpDetail{
		{Bidder: "bidderB", ImpID: "imp1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedOptimized, ReasonDetail: "drop-low-perf"},
		{Bidder: "bidderB", ImpID: "imp2", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedOptimized, ReasonDetail: "drop-low-perf"},
	}, lifecycle.FilteredBidderImpDetails)
}

func TestProcessedAuctionCaptureWithoutLifecycle(t *testing.T) {
	executor := hookexecution.NewHookExecutor(
		excludeBidderPlanBuilder{hook: excludeBidderHook{bidder: "bidderB"}},
		hookexecution.EndpointAuction,
		&metricsConfig.NilMetricsEngine{},
	)
	request := requestWithBidders(t, []string{"imp1"}, "bidderA", "bidderB")

	startProcessedAuctionCapture(nil, executor)
	require.NoError(t, executor.ExecuteProcessedAuctionStage(request))

	impExt, err := request.GetImp()[0].GetImpExt()
	require.NoError(t, err)
	assert.Equal(t, map[string]json.RawMessage{"bidderA": json.RawMessage(`{}`)}, impExt.GetPrebid().Bidder)
}

func TestRequestSplitRecordsPrivacyBlocks(t *testing.T) {
	tests := []struct {
		name           string
		privacyConfig  config.AccountPrivacy
		gdprEnforced   bool
		expectedDetail string
	}{
		{
			name:           "fetch_bids_activity_denied",
			privacyConfig:  getFetchBidsActivityConfig("rubicon", false),
			expectedDetail: filterDetailFetchBidsActivity,
		},
		{
			name:           "gdpr_bid_request_denied",
			gdprEnforced:   true,
			expectedDetail: filterDetailGDPR,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := newBidRequest()
			req.Imp = append(req.Imp, req.Imp[0])
			req.Imp[1].ID = "second-imp-id"
			for i := range req.Imp {
				req.Imp[i].Ext = json.RawMessage(`{"prebid":{"bidder":{"appnexus":{"placementId":1},"rubicon":{}}}}`)
			}
			lifecycle := recordedLifecycle()
			auctionReq := AuctionRequest{
				BidRequestWrapper: &openrtb_ext.RequestWrapper{BidRequest: req},
				UserSyncs:         &emptyUsersync{},
				Activities:        privacy.NewActivityControl(&test.privacyConfig),
				TCF2Config:        gdpr.NewTCF2Config(config.TCF2{}, config.AccountGDPR{}),
				GDPREnforced:      test.gdprEnforced,
				Lifecycle:         lifecycle,
			}
			metricsMock := metrics.MetricsEngineMock{}
			metricsMock.Mock.On("RecordAdapterGDPRRequestBlocked", mock.Anything).Return()
			metricsMock.Mock.On("RecordAdapterBuyerUIDScrubbed", mock.Anything).Return()
			reqSplitter := &requestSplitter{
				bidderToSyncerKey: map[string]string{},
				me:                &metricsMock,
				gdprPermsBuilder: fakePermissionsBuilder{permissions: &permissionsMock{
					allowedBidders: []openrtb_ext.BidderName{openrtb_ext.BidderAppnexus},
					passGeo:        true,
					passID:         true,
				}}.Builder,
				bidderInfo: config.BidderInfos{},
			}

			bidderRequests, _, _ := reqSplitter.cleanOpenRTBRequests(context.Background(), auctionReq, nil, map[string]float64{})

			require.Len(t, bidderRequests, 1)
			assert.Equal(t, openrtb_ext.BidderAppnexus, bidderRequests[0].BidderName)
			assert.Same(t, lifecycle, bidderRequests[0].lifecycle)
			assert.Equal(t, []analytics.FilteredBidderImpDetail{
				{Bidder: "rubicon", ImpID: "some-imp-id", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedPrivacy, ReasonDetail: test.expectedDetail},
				{Bidder: "rubicon", ImpID: "second-imp-id", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedPrivacy, ReasonDetail: test.expectedDetail},
			}, lifecycle.FilteredBidderImpDetails)
		})
	}
}

func TestRecordUnpreparedBidders(t *testing.T) {
	lifecycle := recordedLifecycle()
	lifecycle.RecordFiltered("blocked", "imp1", analytics.RequestBlockedPrivacy, filterDetailGDPR)
	lifecycle.RecordFiltered("failed", "imp2", analytics.RequestBlockedOptimized, "rule")
	impsByBidder := map[string][]openrtb2.Imp{
		"prepared": {{ID: "imp1"}},
		"blocked":  {{ID: "imp1"}},
		"failed":   {{ID: "imp1"}},
	}

	recordUnpreparedBidders(lifecycle, impsByBidder, []BidderRequest{{BidderName: "prepared"}})

	assert.Equal(t, []analytics.FilteredBidderImpDetail{
		{Bidder: "blocked", ImpID: "imp1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedPrivacy, ReasonDetail: filterDetailGDPR},
		{Bidder: "failed", ImpID: "imp2", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedOptimized, ReasonDetail: "rule"},
		{Bidder: "failed", ImpID: "imp1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedGeneral, ReasonDetail: filterDetailRequestPreparationError},
	}, lifecycle.FilteredBidderImpDetails)
}

type rejectingBidderRequestExecutor struct {
	hookexecution.EmptyHookExecutor
}

func (rejectingBidderRequestExecutor) ExecuteBidderRequestStage(*openrtb_ext.RequestWrapper, string) *hookexecution.RejectError {
	return &hookexecution.RejectError{Hook: hookexecution.HookID{ModuleCode: "acme", HookImplCode: "block"}}
}

type noRequestBidder struct{}

func (noRequestBidder) MakeRequests(*openrtb2.BidRequest, *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	return nil, []error{errors.New("missing params")}
}

func (noRequestBidder) MakeBids(*openrtb2.BidRequest, *adapters.RequestData, *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	return nil, nil
}

// impDroppingBidder mimics adapters that reassign request.Imp to the impressions they accept.
type impDroppingBidder struct {
	keep        string
	httpRequest *adapters.RequestData
}

func (b impDroppingBidder) MakeRequests(request *openrtb2.BidRequest, _ *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var kept []openrtb2.Imp
	for _, imp := range request.Imp {
		if imp.ID == b.keep {
			kept = append(kept, imp)
		}
	}
	request.Imp = kept
	if len(kept) == 0 {
		return nil, []error{errors.New("no valid impressions")}
	}
	return []*adapters.RequestData{b.httpRequest}, []error{errors.New("dropped invalid impression")}
}

func (impDroppingBidder) MakeBids(*openrtb2.BidRequest, *adapters.RequestData, *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	return nil, nil
}

func requestBidForLifecycle(t *testing.T, bidderImpl adapters.Bidder, executor hookexecution.StageExecutor, bidderReq BidderRequest) (extraBidderRespInfo, []error) {
	t.Helper()
	bidder := AdaptBidder(bidderImpl, http.DefaultClient, &config.Configuration{}, &metricsConfig.NilMetricsEngine{}, openrtb_ext.BidderAppnexus, nil, "")
	currencyConverter := currency.NewRateConverter(&http.Client{}, time.Duration(1), "", time.Duration(0))
	_, info, errs := bidder.requestBid(context.Background(), bidderReq, currencyConverter.Rates(), &adapters.ExtraRequestInfo{}, &adscert.NilSigner{}, bidRequestOptions{}, openrtb_ext.ExtAlternateBidderCodes{}, executor, nil)
	return info, errs
}

func TestRequestBidRecordsFiltersBeforeRequestsAreBuilt(t *testing.T) {
	siteBannerOnly := config.BidderInfo{Capabilities: &config.CapabilitiesInfo{
		Site: &config.PlatformInfo{MediaTypes: []openrtb_ext.BidType{openrtb_ext.BidTypeBanner}},
	}}
	twoImps := []openrtb2.Imp{{ID: "imp1", Banner: &openrtb2.Banner{}}, {ID: "imp2", Video: &openrtb2.Video{}}}
	tests := []struct {
		name     string
		bidder   adapters.Bidder
		executor hookexecution.StageExecutor
		request  *openrtb2.BidRequest
		expected []analytics.FilteredBidderImpDetail
	}{
		{
			name:     "bidder_request_hook_rejects",
			bidder:   &goodSingleBidder{},
			executor: rejectingBidderRequestExecutor{},
			request:  &openrtb2.BidRequest{Site: &openrtb2.Site{}, Imp: twoImps},
			expected: []analytics.FilteredBidderImpDetail{
				{Bidder: "appnexus", ImpID: "imp1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedGeneral, ReasonDetail: "hook:acme.block"},
				{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedGeneral, ReasonDetail: "hook:acme.block"},
			},
		},
		{
			name:     "unsupported_channel_is_not_also_reported_as_no_request",
			bidder:   adapters.BuildInfoAwareBidder(&goodSingleBidder{}, siteBannerOnly),
			executor: hookexecution.EmptyHookExecutor{},
			request:  &openrtb2.BidRequest{App: &openrtb2.App{}, Imp: twoImps},
			expected: []analytics.FilteredBidderImpDetail{
				{Bidder: "appnexus", ImpID: "imp1", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedUnsupportedChannel, ReasonDetail: "app"},
				{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeBidder, ReasonCode: analytics.RequestBlockedUnsupportedChannel, ReasonDetail: "app"},
			},
		},
		{
			name:     "adapter_builds_no_request",
			bidder:   noRequestBidder{},
			executor: hookexecution.EmptyHookExecutor{},
			request:  &openrtb2.BidRequest{Site: &openrtb2.Site{}, Imp: twoImps},
			expected: []analytics.FilteredBidderImpDetail{
				{Bidder: "appnexus", ImpID: "imp1", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.ErrorGeneral, ReasonDetail: filterDetailNoRequestBuilt},
				{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.ErrorGeneral, ReasonDetail: filterDetailNoRequestBuilt},
			},
		},
		{
			name:     "adapter_drops_every_imp_by_reassigning_request_imp",
			bidder:   impDroppingBidder{keep: "none"},
			executor: hookexecution.EmptyHookExecutor{},
			request:  &openrtb2.BidRequest{Site: &openrtb2.Site{}, Imp: twoImps},
			expected: []analytics.FilteredBidderImpDetail{
				{Bidder: "appnexus", ImpID: "imp1", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.ErrorGeneral, ReasonDetail: filterDetailNoRequestBuilt},
				{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.ErrorGeneral, ReasonDetail: filterDetailNoRequestBuilt},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bidderReq := BidderRequest{BidRequest: test.request, BidderName: "appnexus", lifecycle: recordedLifecycle()}

			info, _ := requestBidForLifecycle(t, test.bidder, test.executor, bidderReq)

			require.NotNil(t, info.lifecycle)
			assert.Equal(t, test.expected, info.lifecycle.FilteredBidderImpDetails)
			assert.Empty(t, info.lifecycle.BidderRequestDetails)
		})
	}
}

func TestRequestBidRecordsMediaTypePruningAndCall(t *testing.T) {
	server := httptest.NewServer(mockHandler(200, "getBody", `{}`))
	defer server.Close()
	bidderImpl := &goodSingleBidder{
		httpRequest: &adapters.RequestData{Method: "POST", Uri: server.URL, Body: []byte(`{"id":"r"}`), ImpIDs: []string{"imp1"}},
		bidResponse: &adapters.BidderResponse{Bids: []*adapters.TypedBid{
			{Bid: &openrtb2.Bid{ID: "bid-1", ImpID: "imp1", Price: 1}, BidType: openrtb_ext.BidTypeBanner},
			{Bid: &openrtb2.Bid{ID: "bid-2", ImpID: "imp1", Price: 2}, BidType: openrtb_ext.BidTypeBanner},
		}},
	}
	siteBannerOnly := config.BidderInfo{Capabilities: &config.CapabilitiesInfo{
		Site: &config.PlatformInfo{MediaTypes: []openrtb_ext.BidType{openrtb_ext.BidTypeBanner}},
	}}
	tests := []struct {
		name         string
		lifecycle    *analytics.AuctionLifecycle
		expectedBody []byte
	}{
		{name: "body_kept_for_selected_bidder", lifecycle: recordedLifecycle("appnexus"), expectedBody: []byte(`{"id":"r"}`)},
		{name: "body_not_kept", lifecycle: recordedLifecycle("rubicon")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bidderReq := BidderRequest{
				BidRequest: &openrtb2.BidRequest{
					TMax: 850,
					Site: &openrtb2.Site{},
					Imp: []openrtb2.Imp{
						{ID: "imp1", Banner: &openrtb2.Banner{}, Native: &openrtb2.Native{}},
						{ID: "imp2", Video: &openrtb2.Video{}},
					},
				},
				BidderName:     "appnexus",
				OpenRTBVersion: "2.6",
				lifecycle:      test.lifecycle,
			}

			info, _ := requestBidForLifecycle(t, adapters.BuildInfoAwareBidder(bidderImpl, siteBannerOnly), hookexecution.EmptyHookExecutor{}, bidderReq)

			require.NotNil(t, info.lifecycle)
			assert.Equal(t, []analytics.FilteredBidderImpDetail{
				{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.RequestBlockedUnsupportedMediaType, ReasonDetail: "mediaType"},
			}, info.lifecycle.FilteredBidderImpDetails)
			require.Len(t, info.lifecycle.BidderRequestDetails, 1)
			call := info.lifecycle.BidderRequestDetails[0]
			call.Duration = 0
			assert.Equal(t, analytics.BidderRequestDetail{
				BidderRequestID: "appnexus-0",
				Bidder:          "appnexus",
				OpenRTBVersion:  "2.6",
				TMaxMs:          850,
				ImpDetails:      []analytics.ImpDetail{{ImpID: "imp1", MediaTypes: []string{"banner"}, RemovedMediaTypes: []string{"native"}, BidIDs: []string{"bid-1", "bid-2"}}},
				HTTPStatus:      200,
				RequestBody:     test.expectedBody,
			}, call)
			assert.Equal(t, []analytics.BidResult{
				{BidderRequestID: "appnexus-0", BidID: "bid-1", ImpID: "imp1"},
				{BidderRequestID: "appnexus-0", BidID: "bid-2", ImpID: "imp1"},
			}, info.lifecycle.Bids)
		})
	}
}

func TestRequestBidWithoutLifecycle(t *testing.T) {
	bidderReq := BidderRequest{BidRequest: &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "imp1"}}}, BidderName: "appnexus"}

	info, _ := requestBidForLifecycle(t, noRequestBidder{}, hookexecution.EmptyHookExecutor{}, bidderReq)

	assert.Nil(t, info.lifecycle)
}

func TestRequestBidRecordsImpDroppedByAdapterAlongsideCall(t *testing.T) {
	server := httptest.NewServer(mockHandler(204, "getBody", ""))
	defer server.Close()
	bidderImpl := impDroppingBidder{
		keep:        "imp1",
		httpRequest: &adapters.RequestData{Method: "POST", Uri: server.URL, ImpIDs: []string{"imp1"}},
	}
	bidderReq := BidderRequest{
		BidRequest: &openrtb2.BidRequest{Site: &openrtb2.Site{}, Imp: []openrtb2.Imp{
			{ID: "imp1", Banner: &openrtb2.Banner{}},
			{ID: "imp2", Video: &openrtb2.Video{}},
		}},
		BidderName: "appnexus",
		lifecycle:  recordedLifecycle(),
	}

	info, _ := requestBidForLifecycle(t, bidderImpl, hookexecution.EmptyHookExecutor{}, bidderReq)

	require.NotNil(t, info.lifecycle)
	assert.Equal(t, []analytics.FilteredBidderImpDetail{
		{Bidder: "appnexus", ImpID: "imp2", Scope: analytics.FilterScopeImpression, ReasonCode: analytics.ErrorGeneral, ReasonDetail: filterDetailNoRequestBuilt},
	}, info.lifecycle.FilteredBidderImpDetails)
	require.Len(t, info.lifecycle.BidderRequestDetails, 1)
	assert.Equal(t, []analytics.ImpDetail{{ImpID: "imp1", MediaTypes: []string{"banner"}}}, info.lifecycle.BidderRequestDetails[0].ImpDetails)
}

func TestRecordUnplacedImpsWhenPlacementUnknown(t *testing.T) {
	lifecycle := &analytics.AuctionLifecycle{}

	recordUnplacedImps(lifecycle, "appnexus", []string{"imp1", "imp2"}, []*adapters.RequestData{{ImpIDs: []string{"imp1"}}, {}})
	recordUnplacedImps(nil, "appnexus", []string{"imp1"}, nil)

	assert.Empty(t, lifecycle.FilteredBidderImpDetails, "a request without ImpIDs makes placement unknown, so nothing is guessed")
}

func TestNotSentReason(t *testing.T) {
	tests := []struct {
		name     string
		info     *httpCallInfo
		expected string
	}{
		{name: "sent", info: &httpCallInfo{request: &adapters.RequestData{Uri: "http://x"}, sent: true}, expected: ""},
		{name: "sent_with_error", info: &httpCallInfo{request: &adapters.RequestData{Uri: "http://x"}, sent: true, err: &errortypes.Timeout{}}, expected: ""},
		{name: "throttled", info: &httpCallInfo{request: &adapters.RequestData{Uri: "http://x"}, err: &errortypes.BidderThrottled{}}, expected: analytics.NotSentThrottled},
		{name: "tmax", info: &httpCallInfo{request: &adapters.RequestData{Uri: "http://x"}, err: &errortypes.TmaxTimeout{}}, expected: analytics.NotSentTmaxInsufficient},
		{name: "request_error", info: &httpCallInfo{request: &adapters.RequestData{Uri: "http://x"}, err: errors.New("bad uri")}, expected: analytics.NotSentRequestError},
		{name: "stored_response", info: &httpCallInfo{request: &adapters.RequestData{}}, expected: analytics.NotSentStoredResponse},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, notSentReason(test.info))
		})
	}
}

func TestRecordDecodedBidsAddsUnlistedImpression(t *testing.T) {
	lifecycle := &analytics.AuctionLifecycle{}
	call := &analytics.BidderRequestDetail{BidderRequestID: "appnexus-0", ImpDetails: []analytics.ImpDetail{{ImpID: "imp1"}}}
	decodeErr := errors.New("partial")

	recordDecodedBids(lifecycle, call, &adapters.BidderResponse{Bids: []*adapters.TypedBid{
		{Bid: &openrtb2.Bid{ID: "a", ImpID: "imp1"}},
		{Bid: &openrtb2.Bid{ID: "b", ImpID: "imp9"}},
		{Bid: nil},
	}}, []error{decodeErr})
	recordDecodedBids(nil, nil, nil, nil)

	assert.Equal(t, []analytics.ImpDetail{{ImpID: "imp1", BidIDs: []string{"a"}}, {ImpID: "imp9", BidIDs: []string{"b"}}}, call.ImpDetails)
	assert.Equal(t, []error{decodeErr}, call.ResponseErrs)
	assert.Equal(t, []analytics.BidResult{
		{BidderRequestID: "appnexus-0", BidID: "a", ImpID: "imp1"},
		{BidderRequestID: "appnexus-0", BidID: "b", ImpID: "imp9"},
	}, lifecycle.Bids)
}

func TestReconcileFinalBidsMarksFilteredBids(t *testing.T) {
	lifecycle := &analytics.AuctionLifecycle{
		Bids: []analytics.BidResult{
			{BidderRequestID: "appnexus-0", BidID: "bid-kept", ImpID: "imp1"},
			{BidderRequestID: "appnexus-0", BidID: "bid-floor", ImpID: "imp1", IsFiltered: true, ReasonCode: analytics.ResponseRejectedBelowFloor, ReasonDetail: "belowFloor"},
			{BidderRequestID: "appnexus-0", BidID: "bid-dropped", ImpID: "imp1"},
		},
	}
	finalResp := &openrtb2.BidResponse{
		SeatBid: []openrtb2.SeatBid{{
			Seat: "appnexus",
			Bid:  []openrtb2.Bid{{ID: "bid-kept", ImpID: "imp1"}},
		}},
	}

	reconcileFinalBids(lifecycle, finalResp)

	assert.Equal(t, []analytics.BidResult{
		{BidderRequestID: "appnexus-0", BidID: "bid-kept", ImpID: "imp1", IsFiltered: false},
		{BidderRequestID: "appnexus-0", BidID: "bid-floor", ImpID: "imp1", IsFiltered: true, ReasonCode: analytics.ResponseRejectedBelowFloor, ReasonDetail: "belowFloor"},
		{BidderRequestID: "appnexus-0", BidID: "bid-dropped", ImpID: "imp1", IsFiltered: true, ReasonCode: analytics.ResponseRejectedGeneral, ReasonDetail: "filtered"},
	}, lifecycle.Bids)
}
