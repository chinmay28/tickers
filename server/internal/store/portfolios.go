package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// portfolioColumns is the one list every portfolio query selects, kept in one
// place so adding a field can't leave a scan and a select disagreeing.
const portfolioColumns = `id, name, allocations, initial_amount, start_year, end_year, rebalance, contribution, contribution_frequency, benchmark, position, created_at, updated_at`

// Portfolios lists every saved allocation in display order.
func (s *Store) Portfolios() ([]Portfolio, error) {
	rows, err := s.db.Query(`
		SELECT ` + portfolioColumns + `
		FROM portfolios ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Portfolio{}
	for rows.Next() {
		p, err := scanPortfolio(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Portfolio looks one up by ID.
func (s *Store) Portfolio(id string) (Portfolio, error) {
	row := s.db.QueryRow(`
		SELECT `+portfolioColumns+`
		FROM portfolios WHERE id = ?`, id)
	p, err := scanPortfolio(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Portfolio{}, ErrNotFound
	}
	return p, err
}

// NewPortfolio is the input for saving an allocation.
type NewPortfolio struct {
	Name                  string
	Holdings              []Holding
	InitialAmount         float64
	StartYear             int
	EndYear               int
	Rebalance             string
	Contribution          float64
	ContributionFrequency string
	Benchmark             string
}

// CreatePortfolio validates and stores an allocation.
func (s *Store) CreatePortfolio(in NewPortfolio) (Portfolio, error) {
	p := Portfolio{
		ID:            newID(),
		Name:          strings.TrimSpace(in.Name),
		Holdings:      in.Holdings,
		InitialAmount: in.InitialAmount,
		StartYear:     in.StartYear,
		EndYear:       in.EndYear,
		Rebalance:     strings.TrimSpace(in.Rebalance),
		Benchmark:     NormalizeSymbol(in.Benchmark),

		Contribution:          in.Contribution,
		ContributionFrequency: strings.TrimSpace(in.ContributionFrequency),
	}
	normalizePortfolio(&p)
	if err := ValidatePortfolio(p); err != nil {
		return Portfolio{}, err
	}

	allocations, err := json.Marshal(p.Holdings)
	if err != nil {
		return Portfolio{}, err
	}
	now := nowRFC3339()
	// Append past the current maximum, exactly as tickers do, so saving a
	// portfolio never renumbers the ones already there.
	if _, err := s.db.Exec(`
		INSERT INTO portfolios (id, name, allocations, initial_amount, start_year, end_year,
		                        rebalance, contribution, contribution_frequency, benchmark,
		                        position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM portfolios), ?, ?)`,
		p.ID, p.Name, string(allocations), p.InitialAmount, p.StartYear, p.EndYear,
		p.Rebalance, p.Contribution, p.ContributionFrequency, p.Benchmark, now, now); err != nil {
		return Portfolio{}, err
	}
	return s.Portfolio(p.ID)
}

// PortfolioPatch is a partial update; a nil field is left alone.
type PortfolioPatch struct {
	Name                  *string
	Holdings              *[]Holding
	InitialAmount         *float64
	StartYear             *int
	EndYear               *int
	Rebalance             *string
	Contribution          *float64
	ContributionFrequency *string
	Benchmark             *string
}

// UpdatePortfolio applies a patch, validating the result as a whole — weights
// are only meaningful as a set, so there is nothing useful to check one at a
// time.
func (s *Store) UpdatePortfolio(id string, patch PortfolioPatch) (Portfolio, error) {
	p, err := s.Portfolio(id)
	if err != nil {
		return Portfolio{}, err
	}
	if patch.Name != nil {
		p.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Holdings != nil {
		p.Holdings = *patch.Holdings
	}
	if patch.InitialAmount != nil {
		p.InitialAmount = *patch.InitialAmount
	}
	if patch.StartYear != nil {
		p.StartYear = *patch.StartYear
	}
	if patch.EndYear != nil {
		p.EndYear = *patch.EndYear
	}
	if patch.Rebalance != nil {
		p.Rebalance = strings.TrimSpace(*patch.Rebalance)
	}
	if patch.Contribution != nil {
		p.Contribution = *patch.Contribution
	}
	if patch.ContributionFrequency != nil {
		p.ContributionFrequency = strings.TrimSpace(*patch.ContributionFrequency)
	}
	if patch.Benchmark != nil {
		p.Benchmark = NormalizeSymbol(*patch.Benchmark)
	}
	normalizePortfolio(&p)
	if err := ValidatePortfolio(p); err != nil {
		return Portfolio{}, err
	}

	allocations, err := json.Marshal(p.Holdings)
	if err != nil {
		return Portfolio{}, err
	}
	if _, err := s.db.Exec(`
		UPDATE portfolios SET name = ?, allocations = ?, initial_amount = ?, start_year = ?,
		                      end_year = ?, rebalance = ?, contribution = ?,
		                      contribution_frequency = ?, benchmark = ?, updated_at = ?
		WHERE id = ?`,
		p.Name, string(allocations), p.InitialAmount, p.StartYear, p.EndYear,
		p.Rebalance, p.Contribution, p.ContributionFrequency, p.Benchmark, nowRFC3339(), id); err != nil {
		return Portfolio{}, err
	}
	return s.Portfolio(id)
}

// DeletePortfolio removes a saved allocation.
func (s *Store) DeletePortfolio(id string) error {
	res, err := s.db.Exec(`DELETE FROM portfolios WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// normalizePortfolio applies the defaults and tidying that both create and
// update want, so a portfolio is in its stored shape before it is validated.
//
// Holdings with no symbol at all are dropped rather than rejected: the editor
// keeps a blank row at the bottom for the next entry, and submitting with it
// still there means "I'm done", not "I meant a holding called nothing".
func normalizePortfolio(p *Portfolio) {
	holdings := make([]Holding, 0, len(p.Holdings))
	for _, h := range p.Holdings {
		h.Symbol = NormalizeSymbol(h.Symbol)
		if h.Symbol == "" && h.Weight == 0 {
			continue
		}
		holdings = append(holdings, h)
	}
	p.Holdings = holdings

	if p.Rebalance == "" {
		p.Rebalance = RebalanceAnnually
	}
	if p.InitialAmount == 0 {
		p.InitialAmount = 10000
	}
	// The two contribution fields only mean anything together, so half of a
	// pair reads as neither: an amount with no cadence has no moment to be paid
	// at, and a cadence with no amount pays nothing at it.
	if p.ContributionFrequency == "" || p.ContributionFrequency == RebalanceNone || p.Contribution <= 0 {
		p.Contribution = 0
		p.ContributionFrequency = RebalanceNone
	}
	if p.Name == "" && len(p.Holdings) > 0 {
		// A portfolio with no name is named after what it holds, which is
		// nearly always what someone would have typed anyway.
		symbols := make([]string, 0, len(p.Holdings))
		for _, h := range p.Holdings {
			symbols = append(symbols, h.Symbol)
		}
		p.Name = strings.Join(symbols, " / ")
	}
}

// ValidatePortfolio rejects an allocation that could not be simulated.
//
// The sum-to-100 rule is the one that earns its keep. Weights that sum to 90
// are not "a portfolio 10% in cash" — nothing here models cash — they are a
// typo, and simulating them would silently answer a different question from
// the one that was asked.
func ValidatePortfolio(p Portfolio) error {
	if p.Name == "" {
		return invalidPortfolio("a portfolio needs a name")
	}
	if len(p.Holdings) == 0 {
		return invalidPortfolio("a portfolio needs at least one holding")
	}
	if len(p.Holdings) > MaxHoldings {
		return invalidPortfolio("a portfolio cannot hold more than %d symbols", MaxHoldings)
	}

	seen := make(map[string]bool, len(p.Holdings))
	total := 0.0
	for _, h := range p.Holdings {
		if h.Symbol == "" {
			return invalidPortfolio("every holding needs a symbol")
		}
		if seen[h.Symbol] {
			return invalidPortfolio("%s is listed twice; give it one combined weight instead", h.Symbol)
		}
		seen[h.Symbol] = true
		if h.Weight <= 0 {
			return invalidPortfolio("%s needs a weight above 0%%", h.Symbol)
		}
		total += h.Weight
	}
	if math.Abs(total-100) > weightTolerance {
		return invalidPortfolio("the weights have to add up to 100%% (they add up to %s)", trimFloat(total))
	}

	if p.InitialAmount <= 0 {
		return invalidPortfolio("the initial amount has to be above 0")
	}
	if !ValidCadence(p.Rebalance) {
		return invalidPortfolio(`rebalance has to be "none", "annually", "quarterly" or "monthly"`)
	}
	if !ValidCadence(p.ContributionFrequency) {
		return invalidPortfolio(`contributionFrequency has to be "none", "annually", "quarterly" or "monthly"`)
	}
	if p.Contribution < 0 {
		return invalidPortfolio("a contribution cannot be negative")
	}
	// Years are sanity-checked, not range-checked against reality: a start
	// before any market data exists is handled by the backtest reporting the
	// date it actually managed to start from.
	if p.StartYear != 0 && (p.StartYear < 1900 || p.StartYear > 2200) {
		return invalidPortfolio("the start year has to be a four-digit year")
	}
	if p.EndYear != 0 && (p.EndYear < 1900 || p.EndYear > 2200) {
		return invalidPortfolio("the end year has to be a four-digit year")
	}
	if p.StartYear != 0 && p.EndYear != 0 && p.EndYear < p.StartYear {
		return invalidPortfolio("the end year cannot be before the start year")
	}
	return nil
}

// portfolioError is one validation failure. It carries its own sentence and
// unwraps to ErrInvalidPortfolio, so the API can answer 400 for the whole
// family without this package having to phrase every message to match the
// marker list in isValidationError — and without prefixing sentences that
// already read as complete ones.
type portfolioError struct{ msg string }

func (e portfolioError) Error() string { return e.msg }
func (e portfolioError) Unwrap() error { return ErrInvalidPortfolio }

func invalidPortfolio(format string, args ...any) error {
	return portfolioError{msg: fmt.Sprintf(format, args...)}
}

// trimFloat renders a weight sum for an error message without the trailing
// zeros that make "99.9899999" out of a number someone typed as three 33.33s.
func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func scanPortfolio(row scannable) (Portfolio, error) {
	var (
		p                    Portfolio
		allocations          string
		createdAt, updatedAt string
	)
	if err := row.Scan(&p.ID, &p.Name, &allocations, &p.InitialAmount, &p.StartYear, &p.EndYear,
		&p.Rebalance, &p.Contribution, &p.ContributionFrequency, &p.Benchmark,
		&p.Position, &createdAt, &updatedAt); err != nil {
		return Portfolio{}, err
	}
	// A row whose JSON won't parse is a database somebody edited by hand. It
	// comes back as a portfolio with no holdings, which the UI shows as empty
	// and offers to fix — better than failing every list query because of one
	// bad row.
	p.Holdings = []Holding{}
	if allocations != "" {
		_ = json.Unmarshal([]byte(allocations), &p.Holdings)
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return p, nil
}
