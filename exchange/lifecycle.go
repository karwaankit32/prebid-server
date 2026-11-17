package exchange

import (
	"strconv"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/analytics"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/exchange/entities"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// Details recorded with analytics.FilteredBidderImpDetail codes.
const (
	filterDetailFetchBidsActivity       = "fetchBidsActivity"
	filterDetailGDPR                    = "gdpr"
	filterDetailRequestPreparationError = "requestPreparationError"
	filterDetailNoRequestBuilt          = "noRequestBuilt"
)

// startProcessedAuctionCapture records bidders removed by processed-auction-request hook mutations.
func startProcessedAuctionCapture(lifecycle *analytics.AuctionLifecycle, executor hookexecution.StageExecutor) {
	if lifecycle == nil {
		return
	}
	if setter, ok := executor.(interface {
		SetBidderRemovalReporter(hookstage.BidderRemovalReporter)
	}); ok {
		setter.SetBidderRemovalReporter(hookFilterReporter{lifecycle: lifecycle})
	}
}

// hookFilterReporter records bidders removed by hooks such as the rules engine, with the hook's reason as detail.
type hookFilterReporter struct {
	lifecycle *analytics.AuctionLifecycle
}

func (r hookFilterReporter) ReportBidderRemoval(impID, bidder, reason string) {
	r.lifecycle.RecordFiltered(bidder, impID, analytics.RequestBlockedOptimized, reason)
}

// capabilityFilterReporter records impressions that one bidder's capability checks remove.
type capabilityFilterReporter struct {
	lifecycle *analytics.AuctionLifecycle
	bidder    string
}

func (r capabilityFilterReporter) ReportImpRemoval(impID string, reason adapters.ImpRemovalReason) {
	code := analytics.RequestBlockedUnsupportedMediaType
	scope := analytics.FilterScopeImpression
	switch reason {
	case adapters.ImpRemovalSiteUnsupported, adapters.ImpRemovalAppUnsupported, adapters.ImpRemovalDOOHUnsupported:
		code = analytics.RequestBlockedUnsupportedChannel
		scope = analytics.FilterScopeBidder
	}
	r.lifecycle.RecordFilteredWithScope(r.bidder, impID, scope, code, string(reason))
}

// filterImps records bidder as filtered from every impression in imps. It is a no-op on a nil lifecycle.
func filterImps(lifecycle *analytics.AuctionLifecycle, bidder string, imps []openrtb2.Imp, code int, detail string) {
	if lifecycle == nil {
		return
	}
	for i := range imps {
		lifecycle.RecordFilteredWithScope(bidder, imps[i].ID, analytics.FilterScopeBidder, code, detail)
	}
}

// impIDs copies the IDs of imps.
func impIDs(imps []openrtb2.Imp) []string {
	ids := make([]string, len(imps))
	for i := range imps {
		ids[i] = imps[i].ID
	}
	return ids
}

// recordUnplacedImps records candidate impressions the adapter put in none of the requests it built,
// skipping those already recorded for bidder. When a built request does not list its impressions,
// placement is unknown and nothing is recorded. It is a no-op on a nil lifecycle.
func recordUnplacedImps(lifecycle *analytics.AuctionLifecycle, bidder string, candidateIDs []string, reqData []*adapters.RequestData) {
	if lifecycle == nil {
		return
	}
	placed := make(map[string]struct{})
	for _, data := range reqData {
		if data == nil || len(data.ImpIDs) == 0 {
			return
		}
		for _, impID := range data.ImpIDs {
			placed[impID] = struct{}{}
		}
	}
	for _, impID := range candidateIDs {
		if _, ok := placed[impID]; !ok && !isFiltered(lifecycle, bidder, impID) {
			lifecycle.RecordFiltered(bidder, impID, analytics.ErrorGeneral, filterDetailNoRequestBuilt)
		}
	}
}

func isFiltered(lifecycle *analytics.AuctionLifecycle, bidder, impID string) bool {
	for _, candidate := range lifecycle.FilteredBidderImpDetails {
		if candidate.Bidder == bidder && candidate.ImpID == impID {
			return true
		}
	}
	return false
}

// recordUnpreparedBidders records bidders that had impressions but got no bidder request from
// cleanOpenRTBRequests without being blocked by privacy, i.e. dropped by a request preparation error.
func recordUnpreparedBidders(lifecycle *analytics.AuctionLifecycle, impsByBidder map[string][]openrtb2.Imp, bidderRequests []BidderRequest) {
	prepared := make(map[string]struct{}, len(bidderRequests))
	for _, bidderRequest := range bidderRequests {
		prepared[string(bidderRequest.BidderName)] = struct{}{}
	}
	for bidder, imps := range impsByBidder {
		if _, ok := prepared[bidder]; ok || isPrivacyBlocked(lifecycle, bidder) {
			continue
		}
		filterImps(lifecycle, bidder, imps, analytics.RequestBlockedGeneral, filterDetailRequestPreparationError)
	}
}

func isPrivacyBlocked(lifecycle *analytics.AuctionLifecycle, bidder string) bool {
	for _, candidate := range lifecycle.FilteredBidderImpDetails {
		if candidate.Bidder == bidder && candidate.ReasonCode == analytics.RequestBlockedPrivacy {
			return true
		}
	}
	return false
}

// newBidderLifecycle returns the lifecycle one bidder records into, or nil when the auction is not recorded.
func newBidderLifecycle(auction *analytics.AuctionLifecycle) *analytics.AuctionLifecycle {
	if auction == nil {
		return nil
	}
	return &analytics.AuctionLifecycle{}
}

func hookRejectDetail(reject *hookexecution.RejectError) string {
	return "hook:" + reject.Hook.ModuleCode + "." + reject.Hook.HookImplCode
}

type impMediaTypeSummary struct {
	remaining []string
	removed   []string
}

// impMediaTypes maps each impression ID to the media types the impression carries.
func impMediaTypes(imps []openrtb2.Imp) map[string][]string {
	mediaTypes := make(map[string][]string, len(imps))
	for i := range imps {
		var types []string
		if imps[i].Banner != nil {
			types = append(types, string(openrtb_ext.BidTypeBanner))
		}
		if imps[i].Video != nil {
			types = append(types, string(openrtb_ext.BidTypeVideo))
		}
		if imps[i].Audio != nil {
			types = append(types, string(openrtb_ext.BidTypeAudio))
		}
		if imps[i].Native != nil {
			types = append(types, string(openrtb_ext.BidTypeNative))
		}
		mediaTypes[imps[i].ID] = types
	}
	return mediaTypes
}

// summarizeImpMediaTypes compares initial impression media types with the post-capability-pruning impressions.
func summarizeImpMediaTypes(initial map[string][]string, currentImps []openrtb2.Imp) map[string]impMediaTypeSummary {
	current := impMediaTypes(currentImps)
	summary := make(map[string]impMediaTypeSummary, len(initial))
	for impID, before := range initial {
		after, ok := current[impID]
		if !ok {
			summary[impID] = impMediaTypeSummary{remaining: before}
			continue
		}
		afterSet := make(map[string]struct{}, len(after))
		for _, mt := range after {
			afterSet[mt] = struct{}{}
		}
		var removed []string
		for _, mt := range before {
			if _, kept := afterSet[mt]; !kept {
				removed = append(removed, mt)
			}
		}
		summary[impID] = impMediaTypeSummary{remaining: after, removed: removed}
	}
	return summary
}

// recordBidderCall appends a record of one built bidder request and returns it, or returns nil when the
// auction is not recorded. The returned pointer is valid only until the next call is recorded.
func recordBidderCall(lifecycle *analytics.AuctionLifecycle, bidderRequest BidderRequest, info *httpCallInfo, impMediaTypes map[string]impMediaTypeSummary) *analytics.BidderRequestDetail {
	if lifecycle == nil || info == nil {
		return nil
	}
	bidder := string(bidderRequest.BidderName)
	var tmaxMs int64
	if bidderRequest.BidRequest != nil {
		tmaxMs = bidderRequest.BidRequest.TMax
	}
	notSent := notSentReason(info)
	errCode, errReason := callErrorCodeAndReason(info, notSent)
	call := analytics.BidderRequestDetail{
		BidderRequestID: bidder + "-" + strconv.Itoa(len(lifecycle.BidderRequestDetails)),
		Bidder:          bidder,
		OpenRTBVersion:  bidderRequest.OpenRTBVersion,
		TMaxMs:          tmaxMs,
		NotSentReason:   notSent,
		ErrorCode:       errCode,
		ErrorReason:     errReason,
		Duration:        info.duration,
		Err:             info.err,
	}
	if info.response != nil {
		call.HTTPStatus = info.response.StatusCode
	}
	if info.request != nil {
		call.ImpDetails = make([]analytics.ImpDetail, len(info.request.ImpIDs))
		for i, impID := range info.request.ImpIDs {
			mt := impMediaTypes[impID]
			call.ImpDetails[i] = analytics.ImpDetail{
				ImpID:             impID,
				MediaTypes:        mt.remaining,
				RemovedMediaTypes: mt.removed,
			}
		}
		if call.NotSentReason != analytics.NotSentStoredResponse && bidderRequest.lifecycle.KeepsRequestBody(bidder) {
			call.RequestBody = info.request.Body
		}
	}
	lifecycle.BidderRequestDetails = append(lifecycle.BidderRequestDetails, call)
	return &lifecycle.BidderRequestDetails[len(lifecycle.BidderRequestDetails)-1]
}

func callErrorCodeAndReason(info *httpCallInfo, notSent string) (int, string) {
	if notSent != "" {
		if notSent == analytics.NotSentStoredResponse {
			return 0, ""
		}
		return analytics.ErrorGeneral, notSent
	}
	if info.err != nil {
		if errortypes.ReadCode(info.err) == errortypes.TimeoutErrorCode {
			return analytics.ErrorTimeout, "timeout"
		}
		return analytics.ErrorGeneral, info.err.Error()
	}
	return 0, ""
}

func notSentReason(info *httpCallInfo) string {
	if info.sent {
		return ""
	}
	if info.request != nil && info.request.Uri == "" {
		return analytics.NotSentStoredResponse
	}
	switch errortypes.ReadCode(info.err) {
	case errortypes.BidderTemporarilyThrottledErrorCode:
		return analytics.NotSentThrottled
	case errortypes.TmaxTimeoutErrorCode:
		return analytics.NotSentTmaxInsufficient
	}
	return analytics.NotSentRequestError
}

// recordDecodedBids adds the bids and errors an adapter decoded from call's response. It is a no-op on a nil call.
func recordDecodedBids(lifecycle *analytics.AuctionLifecycle, call *analytics.BidderRequestDetail, response *adapters.BidderResponse, errs []error) {
	if call == nil {
		return
	}
	call.ResponseErrs = errs
	if call.ErrorCode == 0 && len(errs) > 0 && (response == nil || len(response.Bids) == 0) {
		call.ErrorCode = analytics.ResponseRejectedGeneral
		call.ErrorReason = errs[0].Error()
	}
	if response == nil {
		return
	}
	for _, bid := range response.Bids {
		if bid == nil || bid.Bid == nil {
			continue
		}
		imp := callImp(call, bid.Bid.ImpID)
		imp.BidIDs = append(imp.BidIDs, bid.Bid.ID)
		if lifecycle != nil {
			lifecycle.Bids = append(lifecycle.Bids, analytics.BidResult{
				BidderRequestID: call.BidderRequestID,
				BidID:           bid.Bid.ID,
				ImpID:           bid.Bid.ImpID,
			})
		}
	}
}

// callImp returns call's record for impID, adding one when the bid names an impression the request did not list.
func callImp(call *analytics.BidderRequestDetail, impID string) *analytics.ImpDetail {
	for i := range call.ImpDetails {
		if call.ImpDetails[i].ImpID == impID {
			return &call.ImpDetails[i]
		}
	}
	call.ImpDetails = append(call.ImpDetails, analytics.ImpDetail{ImpID: impID})
	return &call.ImpDetails[len(call.ImpDetails)-1]
}

// reconcileFinalBids marks any decoded bid in lifecycle.Bids that is absent from finalBids as filtered.
func reconcileFinalBids(lifecycle *analytics.AuctionLifecycle, finalResponse *openrtb2.BidResponse) {
	if lifecycle == nil || len(lifecycle.Bids) == 0 {
		return
	}
	surviving := make(map[string]struct{})
	if finalResponse != nil {
		for _, seatBid := range finalResponse.SeatBid {
			for _, bid := range seatBid.Bid {
				surviving[bid.ImpID+"|"+bid.ID] = struct{}{}
			}
		}
	}
	for i := range lifecycle.Bids {
		if lifecycle.Bids[i].IsFiltered {
			continue
		}
		if _, ok := surviving[lifecycle.Bids[i].ImpID+"|"+lifecycle.Bids[i].BidID]; !ok {
			lifecycle.Bids[i].IsFiltered = true
			if lifecycle.Bids[i].ReasonCode == 0 {
				lifecycle.Bids[i].ReasonCode = analytics.ResponseRejectedGeneral
				lifecycle.Bids[i].ReasonDetail = "filtered"
			}
		}
	}
}

// markFilteredBid records a specific post-call filter reason on a decoded bid.
func markFilteredBid(lifecycle *analytics.AuctionLifecycle, bid *entities.PbsOrtbBid, reasonCode int, reasonDetail string) {
	if lifecycle == nil || bid == nil || bid.Bid == nil {
		return
	}
	for i := range lifecycle.Bids {
		if lifecycle.Bids[i].BidID == bid.Bid.ID && lifecycle.Bids[i].ImpID == bid.Bid.ImpID {
			lifecycle.Bids[i].IsFiltered = true
			lifecycle.Bids[i].ReasonCode = reasonCode
			lifecycle.Bids[i].ReasonDetail = reasonDetail
		}
	}
}
