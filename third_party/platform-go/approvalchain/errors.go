package approvalchain

import "errors"

// Definition errors. These surface at registration, not at runtime — a chain
// that cannot route is a deployment bug.
var (
	errBlankChainKey  = errors.New("chain key is required")
	errNoDesks        = errors.New("chain has no desks")
	errBlankDeskKey   = errors.New("desk key is required")
	errDuplicateDesk  = errors.New("desk declared twice in the same chain")
	errNoRoles        = errors.New("desk has no role patterns, so nobody could ever hold it")
	errNegativeBand   = errors.New("desk MinAmount cannot be negative")
	errAllDesksBanded = errors.New("every desk carries a MinAmount, so a small request would approve itself; give one desk MinAmount 0 or set AllowAutoApprove")
)

// Transition errors. Callers map these to HTTP status: ErrNotWaiting and
// ErrWrongDesk are 409, ErrForbidden 403, ErrReasonRequired 400.
var (
	// ErrUnknownChain is returned when a state names a chain the registry does
	// not hold — usually a chain removed while requests were still in flight.
	ErrUnknownChain = errors.New("approvalchain: unknown chain")
	// ErrNotWaiting means the request is not in a state that accepts this action.
	ErrNotWaiting = errors.New("approvalchain: request is not awaiting this action")
	// ErrWrongDesk means the request is sitting on a different desk.
	ErrWrongDesk = errors.New("approvalchain: request is not on this desk")
	// ErrForbidden means the actor's role does not hold the current desk.
	ErrForbidden = errors.New("approvalchain: actor does not hold this desk")
	// ErrSelfApproval is the four-eyes rule: a requester cannot approve their
	// own request unless the desk sets AllowRequester.
	ErrSelfApproval = errors.New("approvalchain: a request cannot be approved by its requester")
	// ErrSelfSuccession is segregation of duties along the chain: on a chain
	// with NoSuccessiveApprover, whoever cleared the previous desk cannot clear
	// this one.
	ErrSelfSuccession = errors.New("approvalchain: a different approver is required to advance this stage")
	// ErrRepeatApprover is segregation of duties across the whole chain: on a
	// chain with NoRepeatApprover, anyone who already cleared a desk on this
	// request cannot clear another.
	ErrRepeatApprover = errors.New("approvalchain: this approver has already signed this request; a different approver is required")
	// ErrReasonRequired is returned when a rejection or amendment omits its
	// reason. Every refusal has to say why — that is the point of the chain.
	ErrReasonRequired = errors.New("approvalchain: a reason is required")
	// ErrNotRequester means only the requester (or an admin) may do this.
	ErrNotRequester = errors.New("approvalchain: only the requester may do this")
	// ErrNoEngagedDesks means bands and skips left nothing to approve on a chain
	// that does not allow auto-approval.
	ErrNoEngagedDesks = errors.New("approvalchain: no desks engage for this request")
	// ErrAmountChanged means the request's amount moved far enough that the desk
	// it was sitting on no longer engages. It must be returned for amendment and
	// re-approved, because the desks that already signed approved a different
	// number.
	ErrAmountChanged = errors.New("approvalchain: the amount changed and the current desk no longer applies; return it for amendment")
	// ErrMissingPermission means the actor holds the desk by role but lacks the
	// permission the desk requires.
	ErrMissingPermission = errors.New("approvalchain: actor lacks the permission this desk requires")
)
