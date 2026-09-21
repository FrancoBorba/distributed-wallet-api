/*
@Author: Franco Ribeiro Borba
@Description: Wallet endpoints. All of them are restricted to the internal
service: a provider moves money through the wagering endpoints and never
touches a wallet directly, which is what keeps a provider from reading the
balance of a player it has no relationship with. The ledger is paginated by an
opaque cursor built from the storage sequence, so the client never learns the
ordering key and the API stays free to change it; the cursor is stable because
the sequence of a row never changes once written. The reconciliation is read
only by design: it reports a divergence and logs it, and never corrects the
balance, because an automatic correction would erase the evidence of whatever
caused the difference.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// Page sizes of the ledger. The maximum bounds one response, so a wallet with
// a long history cannot be asked for in a single query.
const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
)

// WalletHandler serves the wallet endpoints.
type WalletHandler struct {
	open      *usecase.OpenWallet
	reconcile *usecase.ReconcileWallet
	queries   usecase.WalletQueries
	metrics   *metrics.Metrics
	logger    *slog.Logger
}

// NewWalletHandler builds the handler.
func NewWalletHandler(open *usecase.OpenWallet, reconcile *usecase.ReconcileWallet, queries usecase.WalletQueries, logger *slog.Logger, recorder *metrics.Metrics) *WalletHandler {
	return &WalletHandler{open: open, reconcile: reconcile, queries: queries, logger: logger, metrics: recorder}
}

// Open godoc
//
//	@Summary		Open a wallet
//	@Description	Creates the wallet of a player in one currency. A positive initial balance also creates the internal OPENING transaction already processed, its credit entry in the ledger and the corresponding events, all in the same commit. An initial balance of "0.00" creates the wallet alone. The pair (playerId, currency) identifies a wallet, so a second opening is a conflict.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			request			body		OpenWalletRequest	true	"Wallet to open"
//	@Success		201				{object}	WalletResponse
//	@Failure		400				{object}	ErrorResponse	"Invalid input"
//	@Failure		403				{object}	ErrorResponse	"Restricted to the internal service"
//	@Failure		409				{object}	ErrorResponse	"The player already has a wallet in this currency"
//	@Failure		503				{object}	ErrorResponse	"Transient unavailability"
//	@Router			/wallets [post]
func (h *WalletHandler) Open(w http.ResponseWriter, r *http.Request) {
	var request OpenWalletRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "the body is not valid JSON")

		return
	}

	playerID, err := uuid.Parse(request.PlayerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "playerId must be a UUID")

		return
	}

	initialBalance, err := request.InitialBalance.parse()
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())

		return
	}

	wallet, err := h.open.Execute(r.Context(), usecase.OpenWalletCommand{
		PlayerID:       playerID,
		InitialBalance: initialBalance,
		CorrelationID:  correlationOf(r.Context()),
	})
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	writeJSON(w, http.StatusCreated, walletResponseOf(wallet))
}

// Get godoc
//
//	@Summary		Read a wallet
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			walletId		path		string	true	"Wallet identifier"
//	@Success		200				{object}	WalletResponse
//	@Failure		400				{object}	ErrorResponse
//	@Failure		403				{object}	ErrorResponse
//	@Failure		404				{object}	ErrorResponse
//	@Router			/wallets/{walletId} [get]
func (h *WalletHandler) Get(w http.ResponseWriter, r *http.Request) {
	walletID, ok := h.walletID(w, r)
	if !ok {
		return
	}

	wallet, err := h.queries.FindWallet(r.Context(), walletID)
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	writeJSON(w, http.StatusOK, walletResponseOf(wallet))
}

// Ledger godoc
//
//	@Summary		Page the wallet history
//	@Description	Returns the immutable entries of a wallet in a stable order. The cursor is opaque: send back the nextCursor of the previous page and do not interpret it. The absence of nextCursor means the last page was reached.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			walletId		path		string	true	"Wallet identifier"
//	@Param			cursor			query		string	false	"Opaque cursor of the previous page"
//	@Param			limit			query		int		false	"Page size (1-200)"	default(50)
//	@Success		200				{object}	LedgerPageResponse
//	@Failure		400				{object}	ErrorResponse
//	@Failure		403				{object}	ErrorResponse
//	@Failure		404				{object}	ErrorResponse
//	@Router			/wallets/{walletId}/ledger [get]
func (h *WalletHandler) Ledger(w http.ResponseWriter, r *http.Request) {
	walletID, ok := h.walletID(w, r)
	if !ok {
		return
	}

	afterSequence, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "the cursor is not valid")

		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"))

	// The wallet is read first so that an unknown one answers 404 instead of
	// an empty page, which would read as "this wallet has no history".
	if _, err := h.queries.FindWallet(r.Context(), walletID); err != nil {
		fail(w, h.logger, err)

		return
	}

	// One extra row tells us whether another page exists without counting the
	// whole history.
	rows, err := h.queries.PageLedger(r.Context(), walletID, afterSequence, limit+1)
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	response := LedgerPageResponse{
		WalletID: walletID.String(),
		Entries:  make([]LedgerEntryResponse, 0, limit),
	}

	if len(rows) > limit {
		response.NextCursor = encodeCursor(rows[limit-1].Sequence)
		rows = rows[:limit]
	}

	for _, row := range rows {
		response.Entries = append(response.Entries, ledgerEntryResponseOf(row.Entry))
	}

	writeJSON(w, http.StatusOK, response)
}

// Reconcile godoc
//
//	@Summary		Reconcile a wallet against its ledger
//	@Description	Rebuilds the balance from every ledger entry, including the opening credit, and compares it with the stored balance in one consistent view of the data. It never changes the balance. A divergence is reported in the response and in the logs.
//	@Tags			wallets
//	@Security		BearerAuth
//	@Produce		json
//	@Param			walletId		path		string	true	"Wallet identifier"
//	@Success		200				{object}	ReconciliationResponse
//	@Failure		400				{object}	ErrorResponse
//	@Failure		403				{object}	ErrorResponse
//	@Failure		404				{object}	ErrorResponse
//	@Router			/wallets/{walletId}/reconciliation [post]
func (h *WalletHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	walletID, ok := h.walletID(w, r)
	if !ok {
		return
	}

	result, err := h.reconcile.Execute(r.Context(), walletID)
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	// A divergence between the balance and its history is the loudest signal
	// this service can produce: it means money exists that the ledger cannot
	// explain, or the other way around.
	if !result.Consistent {
		h.metrics.ObserveDivergence()

		h.logger.Error("reconciliation divergence",
			slog.String("walletId", walletID.String()),
			slog.String("difference", result.Difference.String()),
			slog.Int("checkedEntries", result.CheckedEntries),
			slog.String("correlationId", correlationOf(r.Context()).String()))
	}

	writeJSON(w, http.StatusOK, reconciliationResponseOf(result))
}

// walletID reads and validates the identifier in the path.
func (h *WalletHandler) walletID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	walletID, err := uuid.Parse(r.PathValue("walletId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "walletId must be a UUID")

		return uuid.Nil, false
	}

	return walletID, true
}

// parseLimit reads the page size, falling back to the default and never going
// past the maximum.
func parseLimit(raw string) int {
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return defaultLedgerLimit
	}
	if limit > maxLedgerLimit {
		return maxLedgerLimit
	}

	return limit
}

// encodeCursor hides the ordering key behind an opaque token.
func encodeCursor(sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(sequence, 10)))
}

// decodeCursor reads a token back. An empty cursor is the first page, which is
// why it is not an error.
func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(string(decoded), 10, 64)
}
