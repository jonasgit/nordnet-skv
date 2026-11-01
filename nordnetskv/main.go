// Command nordnetskv builds SKV 2198 tables ("Genomsnittligt omkostnadsbelopp") and K4
// sales summaries for shares held and sold through Nordnet.
//
// Only the Go standard library is used. It mirrors nordnet_skv.py; see that file for
// the full description of inputs, outputs and the tax treatment used.
//
// Input files (looked up in the given directory):
//
//	Nordnet*.csv           Nordnet "transaktioner och notor" exports (one or more). Every
//	                       security is handled separately; rows sharing a name or ISIN
//	                       are the same security.
//	Omkostnadsbelopp*.csv  optional cost basis for deposits (INLÄGG VP) and opening
//	                       holdings: Värdepapper;Datum;Antal;Omkostnadsbelopp;Händelse;Kommentar
//	                       If missing, mall_omkostnadsbelopp.csv is written to fill in.
//
// Outputs: skv2198.csv, k4.csv and k4_innehav.csv.
//
// ISK and kapitalförsäkring are not declared on K4: leave those accounts out with -depa.
package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	tol          = 0.001
	costPattern  = "Omkostnadsbelopp*.csv"
	templateName = "mall_omkostnadsbelopp.csv"
	transferDays = 7 // max days between UTTAG VP and INLÄGG VP of a move between accounts
)

// Transaction types containing any of these do not affect the omkostnadsbelopp.
var ignored = []string{"UTDELNING", "SKATT", "RÄNTA", "AVGIFT", "KUPONG"}

var warnings []string

func warn(format string, a ...any) { warnings = append(warnings, fmt.Sprintf(format, a...)) }

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------- dates

// Date is a calendar date (UTC midnight).
type Date struct{ time.Time }

func (d Date) String() string     { return d.Format("2006-01-02") }
func (d Date) IsZero() bool       { return d.Time.IsZero() }
func (d Date) Before(o Date) bool { return d.Time.Before(o.Time) }
func (d Date) AddDays(n int) Date { return Date{d.Time.AddDate(0, 0, n)} }
func (d Date) Year4() string      { return d.Format("2006") }
func (d Date) Cmp(o Date) int     { return d.Time.Compare(o.Time) }
func (d Date) DaysTo(o Date) int  { return int(math.Round(o.Time.Sub(d.Time).Hours() / 24)) }

func parseISO(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	return Date{t}, err
}

// ---------------------------------------------------------------------------- numbers

// num parses "1 234,56", "1234.56", "-" or "" (the latter two give ok=false).
func num(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" || s == "-" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func numOr0(s string) float64 { f, _ := num(s); return f }

// g formats like Python's "{:g}".
func g(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

func fmtPlain(f float64) string { return strings.ReplaceAll(g(f), ".", ",") }

// fmtSv formats with thousands separator ' ' and decimal comma.
func fmtSv(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i+1:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if frac != "" {
		out += "," + frac
	}
	if neg && strings.Trim(out, "0, ") != "" {
		out = "-" + out
	}
	return out
}

// ---------------------------------------------------------------------------- CSV

func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}), bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		le := b[0] == 0xff
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			if le {
				u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
			} else {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			}
		}
		return string(utf16.Decode(u))
	case bytes.HasPrefix(b, []byte("\xef\xbb\xbf")):
		return string(b[3:])
	case utf8.Valid(b):
		return string(b)
	default: // Latin-1 / cp1252 approximation
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
}

// readTable reads a tab/semicolon/comma separated file into maps. A repeated header
// name (Nordnet has several "Valuta" columns) is renamed "<name> (<previous column>)",
// e.g. "Valuta (Belopp)".
func readTable(path string) []map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	text := strings.ReplaceAll(decodeText(b), "\r\n", "\n")
	first, _, _ := strings.Cut(text, "\n")
	r := csv.NewReader(strings.NewReader(text))
	switch {
	case strings.Contains(first, "\t"):
		r.Comma = '\t'
	case strings.Contains(first, ";"):
		r.Comma = ';'
	default:
		r.Comma = ','
	}
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		fatal("%s: %v", path, err)
	}
	if len(recs) == 0 {
		return nil
	}
	var header []string
	seen := map[string]bool{}
	for _, h := range recs[0] {
		h = strings.TrimSpace(h)
		name := h
		if seen[h] && len(header) > 0 {
			name = fmt.Sprintf("%s (%s)", h, header[len(header)-1])
		}
		seen[h] = true
		header = append(header, name)
	}
	var out []map[string]string
	for _, rec := range recs[1:] {
		empty := true
		for _, x := range rec {
			if strings.TrimSpace(x) != "" {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		m := map[string]string{}
		for i, name := range header {
			if i < len(rec) {
				m[name] = strings.TrimSpace(rec[i])
			}
		}
		out = append(out, m)
	}
	return out
}

func find(dir, pattern string) []string {
	hits, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(hits)
	return hits
}

// ---------------------------------------------------------------------------- model

// Event kinds: Köp, Förm, Övr, S, Förs, Ut (withdrawal at average cost, shown as Övr).
var order = map[string]int{"S": 0, "Köp": 1, "Förm": 2, "Övr": 2, "Ut": 3, "Förs": 3}

type Event struct {
	Date     Date
	Kind     string
	Qty      float64 // shares in (+); for Förs/Ut the number of shares out (positive)
	Cost     float64 // added omkostnadsbelopp (Köp/Förm/Övr)
	Proceeds float64 // Förs: net sale amount after fees
	Ratio    float64 // S: new shares per old share
	Note     string
	Ref      string
}

func (e Event) Order() int { return order[e.Kind] }

func sortEvents(ev []Event) {
	sort.SliceStable(ev, func(i, j int) bool {
		if c := ev[i].Date.Cmp(ev[j].Date); c != 0 {
			return c < 0
		}
		return ev[i].Order() < ev[j].Order()
	})
}

type Deposit struct {
	Date        Date
	Qty         float64
	Text        string
	Depa        string
	Kind        string // Nordnet transaktionstyp
	NordnetCost float64
	HasCost     bool
	Internal    bool // move between own accounts
}

type Security struct {
	Name        string
	Names       map[string]bool // upper-cased
	Isins       map[string]bool // upper-cased
	Events      []Event
	Deposits    []*Deposit
	StartDate   Date
	Opening     float64 // holdings just before StartDate (units of that date)
	Closing     float64
	ClosingDate Date
	Eod         map[Date]float64 // holdings at end of each trade date
	InTransit   [][2]Date
}

func (s *Security) matches(key string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	return s.Isins[k] || s.Names[k]
}

type CostRow struct {
	Security string
	Date     Date
	Qty      float64
	Cost     float64
	HasCost  bool
	Kind     string
	Note     string
}

// ---------------------------------------------------------------------------- Nordnet

func tdate(r map[string]string) Date {
	s := r["Affärsdag"]
	if s == "" {
		s = r["Bokföringsdag"]
	}
	d, err := parseISO(s)
	if err != nil {
		fatal("Nordnet: ogiltigt datum %q", s)
	}
	return d
}

func ttype(r map[string]string) string {
	return strings.ToUpper(strings.TrimSpace(r["Transaktionstyp"]))
}

func isIgnored(t string) bool {
	for _, x := range ignored {
		if strings.Contains(t, x) {
			return true
		}
	}
	return false
}

// delta returns the holding change of a row; ok=false for an unknown type.
func delta(t string, q float64) (float64, bool) {
	switch {
	case t == "KÖPT":
		return q, true
	case t == "SÅLT":
		return -q, true
	case strings.Contains(t, "INLÄGG"):
		return q, true
	case strings.Contains(t, "UTTAG"):
		return -q, true
	case isIgnored(t):
		return 0, true
	}
	return 0, false
}

var ratioRe = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*:\s*(\d+(?:[.,]\d+)?)`)

// splitRatio parses "Split ABC B, 4:1" -> 4 (new shares per old share).
func splitRatio(text string) (float64, bool) {
	m := ratioRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	a, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	b, _ := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", "."), 64)
	if a == 0 || b == 0 {
		return 0, false
	}
	return a / b, true
}

func rowKeys(r map[string]string) []string {
	k := []string{strings.ToUpper(r["Värdepapper"])}
	if r["ISIN"] != "" {
		k = append(k, strings.ToUpper(r["ISIN"]))
	}
	return k
}

// loadNordnet returns the securities in the exports (only those matching `only`, names
// or ISINs, if given) and the upper-cased names and ISINs of all securities.
func loadNordnet(paths, depas, only []string) ([]*Security, map[string]bool) {
	seen := map[string]bool{}
	depaOK := map[string]bool{}
	for _, d := range depas {
		depaOK[d] = true
	}
	var rows []map[string]string
	for _, p := range paths {
		for _, r := range readTable(p) {
			if r["Värdepapper"] == "" || r["Makuleringsdatum"] != "" {
				continue
			}
			if len(depas) > 0 && !depaOK[r["Depå"]] {
				continue
			}
			key := r["Id"]
			if key == "" {
				var parts []string
				for k, v := range r {
					parts = append(parts, k+"="+v)
				}
				sort.Strings(parts)
				key = strings.Join(parts, "|")
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, r)
		}
	}

	// Group rows into securities: rows sharing a name or an ISIN belong together.
	parent := map[string]string{}
	var root func(string) string
	root = func(x string) string {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, r := range rows {
		a := root("N:" + strings.ToUpper(r["Värdepapper"]))
		if r["ISIN"] != "" {
			parent[root("I:"+strings.ToUpper(r["ISIN"]))] = a
		}
	}
	groups := map[string][]map[string]string{}
	var groupOrder []string
	for _, r := range rows {
		k := root("N:" + strings.ToUpper(r["Värdepapper"]))
		if _, ok := groups[k]; !ok {
			groupOrder = append(groupOrder, k)
		}
		groups[k] = append(groups[k], r)
	}

	known := map[string]bool{}
	for _, r := range rows {
		for _, k := range rowKeys(r) {
			known[k] = true
		}
	}
	wanted := map[string]bool{}
	var missing []string
	for _, k := range only {
		k = strings.ToUpper(strings.TrimSpace(k))
		if !wanted[k] && !known[k] {
			missing = append(missing, k)
		}
		wanted[k] = true
	}
	sort.Strings(missing)
	for _, k := range missing {
		warn("--security %s: finns inte i Nordnet-exporten.", k)
	}

	rid := func(r map[string]string) int64 {
		n, _ := strconv.ParseInt(r["Id"], 10, 64)
		return n
	}
	var out []*Security
	for _, gk := range groupOrder {
		grp := groups[gk]
		if len(wanted) > 0 {
			hit := false
			for _, r := range grp {
				for _, k := range rowKeys(r) {
					hit = hit || wanted[k]
				}
			}
			if !hit {
				continue
			}
		}
		// Same day: SPLIT UTTAG before SPLIT INLÄGG, otherwise by Id.
		splitFirst := func(r map[string]string) int {
			if ttype(r) == "SPLIT UTTAG VP" {
				return 0
			}
			return 1
		}
		sort.SliceStable(grp, func(i, j int) bool {
			if c := tdate(grp[i]).Cmp(tdate(grp[j])); c != 0 {
				return c < 0
			}
			if a, b := splitFirst(grp[i]), splitFirst(grp[j]); a != b {
				return a < b
			}
			return rid(grp[i]) < rid(grp[j])
		})
		out = append(out, processSecurity(grp))
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToUpper(out[i].Name) < strings.ToUpper(out[j].Name) })
	return out, known
}

func processSecurity(rows []map[string]string) *Security {
	s := &Security{Name: rows[len(rows)-1]["Värdepapper"], Names: map[string]bool{}, Isins: map[string]bool{},
		Eod: map[Date]float64{}, StartDate: tdate(rows[0])}
	hold := map[string]float64{} // holdings per Nordnet account (Depå)
	var depaOrder []string
	sumHold := func() float64 {
		t := 0.0
		for _, d := range depaOrder {
			t += hold[d]
		}
		return t
	}
	splitOut, splitIn := map[Date]float64{}, map[Date]float64{}
	splitText := map[Date]string{}
	var splitDates []Date
	var withdrawals []*Deposit
	currencies := map[string]bool{}
	unknown := map[string]bool{}

	for _, r := range rows {
		s.Names[strings.ToUpper(r["Värdepapper"])] = true
		if r["ISIN"] != "" {
			s.Isins[strings.ToUpper(r["ISIN"])] = true
		}
		t, d := ttype(r), tdate(r)
		q := numOr0(r["Antal"])
		depa := r["Depå"]
		text := r["Transaktionstext"]
		dq, ok := delta(t, q)
		if !ok {
			unknown[r["Transaktionstyp"]] = true
		}
		if _, ok := hold[depa]; !ok {
			// Holding before the first row of this account. Dividend rows have
			// "Totalt antal" 0 but the holding in "Antal".
			after, hasAfter := num(r["Totalt antal"])
			switch {
			case strings.Contains(t, "UTDELNING"):
				hold[depa] = q
			case hasAfter && !isIgnored(t):
				hold[depa] = after - dq
			default:
				hold[depa] = 0
			}
			depaOrder = append(depaOrder, depa)
			s.Opening += hold[depa]
		}

		switch {
		case strings.Contains(t, "SPLIT"):
			if _, ok := splitText[d]; !ok {
				splitText[d] = text
				splitDates = append(splitDates, d)
			}
			if strings.Contains(t, "INLÄGG") {
				splitIn[d] += q
			} else {
				splitOut[d] += q
			}
		case t == "KÖPT" || t == "SÅLT":
			if cur := r["Valuta (Belopp)"]; cur != "" {
				currencies[cur] = true
			}
			amount := numOr0(r["Belopp"])
			ref := r["Notanummer"]
			if t == "KÖPT" {
				s.Events = append(s.Events, Event{Date: d, Kind: "Köp", Qty: q, Cost: -amount,
					Note: fmt.Sprintf("Nordnet köpt %s st", g(q)), Ref: ref})
			} else {
				fee := numOr0(r["Total Avgift"])
				price := numOr0(r["Kurs"])
				s.Events = append(s.Events, Event{Date: d, Kind: "Förs", Qty: q, Proceeds: amount, Ref: ref,
					Note: fmt.Sprintf("Nordnet sålt %s st à %s, avgift %s", g(q), g(price), g(fee))})
			}
		case strings.Contains(t, "INLÄGG"):
			c, has := num(r["Inköpsvärde"])
			s.Deposits = append(s.Deposits, &Deposit{Date: d, Qty: q, Text: text, Depa: depa, Kind: t,
				NordnetCost: c, HasCost: has})
		case strings.Contains(t, "UTTAG"):
			withdrawals = append(withdrawals, &Deposit{Date: d, Qty: q, Text: text, Depa: depa, Kind: t})
		}

		hold[depa] += dq
		if tot, ok := num(r["Totalt antal"]); ok && !isIgnored(t) && math.Abs(tot-hold[depa]) > tol {
			warn("%s %s: beräknat innehav %s i depå %s men 'Totalt antal' %s.", s.Name, d, g(hold[depa]), depa, g(tot))
			hold[depa] = tot
		}
		s.Eod[d] = sumHold()
	}
	s.Closing = sumHold()
	s.ClosingDate = tdate(rows[len(rows)-1])
	for _, u := range sortedKeys(unknown) {
		warn("%s: okänd transaktionstyp '%s' ignorerad.", s.Name, u)
	}
	for _, c := range sortedKeys(currencies) {
		if c != "SEK" {
			warn("%s: belopp i %s - K4 och SKV 2198 ska vara i kronor, räkna om manuellt.", s.Name, c)
		}
	}

	// Splits.
	sort.Slice(splitDates, func(i, j int) bool { return splitDates[i].Before(splitDates[j]) })
	for _, d := range splitDates {
		rText, hasText := splitRatio(splitText[d])
		rQty, hasQty := 0.0, splitOut[d] != 0
		if hasQty {
			rQty = splitIn[d] / splitOut[d]
		}
		var ratio float64
		switch {
		case hasText && (!hasQty || math.Abs(rQty/rText-1) < 0.05):
			ratio = rText
		case hasText && hasQty && rQty != 0 && math.Abs(rQty*rText-1) < 0.05:
			ratio = 1 / rText
		case hasQty && rQty != 0:
			ratio = rQty
			if hasText {
				warn("%s %s: splitkvot enligt texten (%s) stämmer inte med antalen - %s används.",
					s.Name, d, splitText[d], g(ratio))
			}
		default:
			warn("%s %s: split utan känd kvot ('%s') - ignoreras.", s.Name, d, splitText[d])
			continue
		}
		label := g(ratio) + ":1"
		if ratio < 1 {
			label = "1:" + g(1/ratio)
		}
		s.Events = append(s.Events, Event{Date: d, Kind: "S", Ratio: ratio, Note: "Split " + label})
	}

	// Moves between own accounts: UTTAG VP in one account, INLÄGG VP of the same quantity
	// in another within transferDays.
	for _, w := range withdrawals {
		if strings.Contains(w.Kind, "OMVANDLING") {
			continue
		}
		for _, dp := range s.Deposits {
			days := w.Date.DaysTo(dp.Date)
			if !dp.Internal && dp.Depa != w.Depa && math.Abs(dp.Qty-w.Qty) < tol &&
				days <= transferDays && days >= -transferDays {
				w.Internal, dp.Internal = true, true
				a, b := w.Date, dp.Date
				if b.Before(a) {
					a, b = b, a
				}
				s.InTransit = append(s.InTransit, [2]Date{a, b})
				break
			}
		}
	}
	for _, w := range withdrawals {
		if w.Internal {
			continue
		}
		s.Events = append(s.Events, Event{Date: w.Date, Kind: "Ut", Qty: w.Qty,
			Note: fmt.Sprintf("Nordnet %s %s st ('%s'), tas bort till genomsnittligt omkostnadsbelopp",
				strings.ToLower(w.Kind), g(w.Qty), w.Text)})
		if !strings.Contains(w.Kind, "OMVANDLING") {
			warn("%s %s: %s %s st ('%s') - aktierna flyttades från Nordnet och tas bort till "+
				"genomsnittligt omkostnadsbelopp. Säljs de någon annanstans gäller samma genomsnitt.",
				s.Name, w.Date, w.Kind, g(w.Qty), w.Text)
		}
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// ---------------------------------------------------------------------------- cost basis

func loadCosts(path string) []*CostRow {
	base := filepath.Base(path)
	var out []*CostRow
	for i, r := range readTable(path) {
		line := i + 2
		d, err := parseISO(r["Datum"])
		if err != nil {
			warn("%s rad %d: ogiltigt datum '%s' - raden hoppas över.", base, line, r["Datum"])
			continue
		}
		q, ok := num(r["Antal"])
		if !ok || q == 0 {
			warn("%s rad %d: Antal saknas - raden hoppas över.", base, line)
			continue
		}
		sec := strings.TrimSpace(r["Värdepapper"])
		if sec == "" {
			warn("%s rad %d: Värdepapper saknas - raden hoppas över.", base, line)
			continue
		}
		kind := strings.TrimSpace(r["Händelse"])
		if kind == "" {
			kind = "Köp"
		}
		if rs := []rune(strings.ToLower(kind)); len(rs) > 0 {
			kind = strings.ToUpper(string(rs[0])) + string(rs[1:])
		}
		if kind != "Köp" && kind != "Förm" && kind != "Övr" {
			warn("%s rad %d: okänd Händelse '%s', Övr används.", base, line, kind)
			kind = "Övr"
		}
		cost, hasCost := num(r["Omkostnadsbelopp"])
		out = append(out, &CostRow{sec, d, q, cost, hasCost, kind, r["Kommentar"]})
	}
	return out
}

// depositEvents turns a security's deposits (and its opening holding) into ledger
// events using the cost file, else Nordnet's inköpsvärde.
func depositEvents(s *Security, costs []*CostRow, haveFile bool) []Event {
	var events []Event
	start := s.StartDate
	byDate := map[Date][]*CostRow{}
	for _, c := range costs {
		byDate[c.Date] = append(byDate[c.Date], c)
	}
	fromRows := func(rows []*CostRow, prefix string) float64 {
		total := 0.0
		for _, c := range rows {
			if !c.HasCost {
				warn("%s %s: %s st i kostnadsfilen saknar omkostnadsbelopp - 0 kr används.", s.Name, c.Date, g(c.Qty))
			}
			note := prefix
			if c.Note != "" {
				note += ", " + c.Note
			}
			events = append(events, Event{Date: c.Date, Kind: c.Kind, Qty: c.Qty, Cost: c.Cost, Note: note})
			total += c.Qty
		}
		return total
	}

	// Opening holding (shares in Nordnet before the export starts).
	var before []*CostRow
	for _, c := range costs {
		if c.Date.Before(start) {
			before = append(before, c)
		}
	}
	sort.SliceStable(before, func(i, j int) bool { return before[i].Date.Before(before[j].Date) })
	got := fromRows(before, "Ingående innehav enligt kostnadsfilen")
	if (s.Opening > tol || got > tol) && math.Abs(got-s.Opening) > tol {
		if missing := s.Opening - got; missing > 0 {
			events = append(events, Event{Date: start.AddDays(-1), Kind: "Övr", Qty: missing,
				Note: "Ingående innehav utan känt omkostnadsbelopp"})
		}
		warn("%s: Nordnet-exporten börjar %s med %s st men kostnadsfilen har %s st daterade före det - "+
			"ange ingående innehav med omkostnadsbelopp (rader daterade före exportens början).",
			s.Name, start, g(s.Opening), g(got))
	}

	// Deposits, grouped per date.
	deposited := map[Date][]*Deposit{}
	var dates []Date
	for _, dp := range s.Deposits {
		if dp.Internal {
			continue
		}
		if _, ok := deposited[dp.Date]; !ok {
			dates = append(dates, dp.Date)
		}
		deposited[dp.Date] = append(deposited[dp.Date], dp)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	var missingDates []string
	for _, d := range dates {
		items := deposited[d]
		qty := 0.0
		texts := map[string]bool{}
		for _, x := range items {
			qty += x.Qty
			if x.Text != "" {
				texts[x.Text] = true
			}
		}
		prefix := fmt.Sprintf("Nordnet inlägg %s st", g(qty))
		if len(texts) > 0 {
			prefix += fmt.Sprintf(" ('%s')", strings.Join(sortedKeys(texts), ", "))
		}
		rows := byDate[d]
		got := fromRows(rows, prefix)
		rest := qty - got
		if rest > tol && len(rows) == 0 {
			// No cost file rows: use Nordnet's inköpsvärde where it has one.
			for _, x := range items {
				if x.HasCost {
					events = append(events, Event{Date: d, Kind: "Övr", Qty: x.Qty, Cost: x.NordnetCost,
						Note: fmt.Sprintf("%s, inköpsvärde %s enligt Nordnet", prefix, g(x.NordnetCost))})
					warn("%s %s: %s %s st medtagen med inköpsvärde %s kr enligt Nordnet - kontrollera.",
						s.Name, d, x.Kind, g(x.Qty), g(x.NordnetCost))
					rest -= x.Qty
				}
			}
		}
		if rest > tol {
			events = append(events, Event{Date: d, Kind: "Övr", Qty: rest,
				Note: prefix + ", omkostnadsbelopp saknas (0 kr)"})
			if len(rows) > 0 {
				warn("%s %s: inlägg %s st men kostnadsfilen har %s st - %s st tas med till 0 kr.",
					s.Name, d, g(qty), g(got), g(rest))
			} else {
				missingDates = append(missingDates, d.String())
			}
		} else if got-qty > tol {
			warn("%s %s: kostnadsfilen har %s st men inlägget är %s st.", s.Name, d, g(got), g(qty))
		}
	}
	if len(missingDates) > 0 {
		if !haveFile {
			warn("%s: ingen kostnadsfil (%s) - %d inlägg tas med till 0 kr. Fyll i %s och spara den "+
				"som Omkostnadsbelopp.csv.", s.Name, costPattern, len(missingDates), templateName)
		} else {
			warn("%s: %d inlägg saknas i kostnadsfilen och tas med till 0 kr: %s",
				s.Name, len(missingDates), strings.Join(missingDates, ", "))
		}
	}

	var costDates []Date
	for d := range byDate {
		costDates = append(costDates, d)
	}
	sort.Slice(costDates, func(i, j int) bool { return costDates[i].Before(costDates[j]) })
	for _, d := range costDates {
		if d.Before(start) || deposited[d] != nil {
			continue
		}
		sum := 0.0
		for _, c := range byDate[d] {
			sum += c.Qty
		}
		warn("Kostnadsfilen %s %s: %s st matchar inget inlägg i Nordnet - ignoreras.", s.Name, d, g(sum))
	}
	return events
}

func writeTemplate(path string, secs []*Security) {
	var rows [][]string
	for _, s := range secs {
		if s.Opening > tol {
			rows = append(rows, []string{s.Name, s.StartDate.AddDays(-1).String(), fmtPlain(s.Opening), "",
				"Köp", "Ingående innehav före Nordnet-exporten"})
		}
		var deps []*Deposit
		for _, dp := range s.Deposits {
			if !dp.Internal {
				deps = append(deps, dp)
			}
		}
		sort.SliceStable(deps, func(i, j int) bool { return deps[i].Date.Before(deps[j].Date) })
		for _, dp := range deps {
			cost, kind := "", "Köp"
			if dp.HasCost {
				cost, kind = fmtPlain(dp.NordnetCost), "Övr"
			}
			rows = append(rows, []string{s.Name, dp.Date.String(), fmtPlain(dp.Qty), cost, kind, ""})
		}
	}
	writeCSV(path, []string{"Värdepapper", "Datum", "Antal", "Omkostnadsbelopp", "Händelse", "Kommentar"}, rows, true)
}

// ---------------------------------------------------------------------------- calculation

type Row struct {
	Name      string
	Date      Date
	Kind      string
	Delta     float64
	TotalQty  float64
	Change    float64
	TotalCost float64
	Avg       float64
	Note      string
}

type Sale struct {
	Date     Date
	Qty      float64
	Name     string
	Proceeds float64
	Cost     float64
	Result   float64
	Ref      string
}

func compute(events []Event, name string) ([]Row, []Sale) {
	ev := append([]Event(nil), events...)
	sortEvents(ev)
	var rows []Row
	var sales []Sale
	totalQty, totalCost := 0.0, 0.0
	for _, e := range ev {
		var dq, change float64
		switch e.Kind {
		case "S":
			dq = totalQty * (e.Ratio - 1)
		case "Förs", "Ut":
			if e.Qty > totalQty+tol {
				what := "uttag"
				if e.Kind == "Förs" {
					what = "försäljning"
				}
				warn("%s %s: %s av %s st men endast %s st i beräkningen - historiken är ofullständig.",
					name, e.Date, what, g(e.Qty), g(totalQty))
			}
			avgBefore := 0.0
			if totalQty != 0 {
				avgBefore = totalCost / totalQty
			}
			dq, change = -e.Qty, -e.Qty*avgBefore+0 // +0 avoids -0
			if e.Kind == "Förs" {
				sales = append(sales, Sale{Date: e.Date, Qty: e.Qty, Name: name, Proceeds: e.Proceeds,
					Cost: 0 - change, Result: e.Proceeds + change, Ref: e.Ref})
			}
		default:
			dq, change = e.Qty, e.Cost
		}
		totalQty += dq
		totalCost += change
		if math.Abs(totalQty) < tol {
			totalQty, totalCost = 0, 0
		}
		avg := 0.0
		if totalQty != 0 {
			avg = totalCost / totalQty
		}
		kind := e.Kind
		if kind == "Ut" {
			kind = "Övr"
		}
		rows = append(rows, Row{name, e.Date, kind, dq, totalQty, change, totalCost, avg, e.Note})
	}
	return rows, sales
}

// ---------------------------------------------------------------------------- holdings log

var logLabel = map[string]string{"S": "Split", "Köp": "Köp", "Förm": "Förm", "Övr": "Övr", "Förs": "Försäljning"}

var logHeader = []string{"Inkomstår", "Datum", "Händelse", "Antal", "Beteckning",
	"Försäljningspris", "Omkostnadsbelopp", "Vinst", "Förlust", "Nota", "Kommentar", "Kvar"}

// buildLog returns one row per SKV 2198 event with K4 values on sales and the holding
// after it, and cross-checks the holding against Nordnet's "Totalt antal".
func buildLog(s *Security, rows []Row, sales []Sale, nf numFmt) [][]string {
	var out [][]string
	eod := map[Date]float64{}
	si := 0
	for _, r := range rows {
		eod[r.Date] = r.TotalQty
		cells := []string{r.Date.Year4(), r.Date.String(), logLabel[r.Kind], nf.f(r.Delta, 5), r.Name,
			"", "", "", "", "", r.Note, nf.f(r.TotalQty, 5)}
		if r.Kind == "Förs" {
			sale := sales[si]
			si++
			cells[5] = nf.f(sale.Proceeds, 0)
			cells[6] = nf.f(sale.Cost, 0)
			cells[7] = nf.f(math.Max(sale.Result, 0), 0)
			cells[8] = nf.f(math.Max(-sale.Result, 0), 0)
			cells[9] = sale.Ref
		}
		out = append(out, cells)
	}
	var days []Date
	for d := range s.Eod {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
outer:
	for _, d := range days {
		for _, tr := range s.InTransit {
			if !d.Before(tr[0]) && d.Before(tr[1]) {
				continue outer
			}
		}
		if calc, ok := eod[d]; ok && math.Abs(calc-s.Eod[d]) > tol {
			warn("%s %s: beräknat innehav %s st, enligt Nordnet %s st (första avvikelsen).",
				s.Name, d, g(calc), g(s.Eod[d]))
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------- output

type numFmt struct {
	swedish bool
}

func (n numFmt) f(v float64, dec int) string {
	if n.swedish {
		return strings.ReplaceAll(fmtSv(v, dec), " ", "")
	}
	return strconv.FormatFloat(v, 'f', dec, 64)
}

func writeCSV(path string, header []string, rows [][]string, swedish bool) {
	f, err := os.Create(path)
	if err != nil {
		fatal("%s: %v", path, err)
	}
	defer f.Close()
	f.WriteString("\ufeff")
	w := csv.NewWriter(f)
	w.Comma = ','
	if swedish {
		w.Comma = ';'
	}
	w.UseCRLF = true
	w.Write(header)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		fatal("%s: %v", path, err)
	}
}

func printTable(headers []string, data [][]string, left map[int]bool) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
		for _, d := range data {
			if n := utf8.RuneCountInString(d[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}
	line := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
			if left[i] {
				parts[i] = c + pad
			} else {
				parts[i] = pad + c
			}
		}
		return strings.Join(parts, "  ")
	}
	fmt.Println(line(headers))
	dashes := make([]string, len(widths))
	for i, w := range widths {
		dashes[i] = strings.Repeat("-", w)
	}
	fmt.Println(strings.Join(dashes, "  "))
	for _, d := range data {
		fmt.Println(line(d))
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// splitArgs lets flags appear after the positional directory argument.
func splitArgs(args []string, fs *flag.FlagSet) (flagsPart, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flagsPart = append(flagsPart, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				continue
			}
			if f := fs.Lookup(name); f != nil {
				if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
					continue
				}
				if i+1 < len(args) {
					i++
					flagsPart = append(flagsPart, args[i])
				}
			}
			continue
		}
		positional = append(positional, a)
	}
	return
}

func main() {
	fs := flag.NewFlagSet("nordnetskv", flag.ExitOnError)
	output := fs.String("o", "skv2198.csv", "SKV 2198 CSV")
	k4Path := fs.String("k4", "k4.csv", "K4 sales CSV")
	k4HoldPath := fs.String("k4-holdings", "k4_innehav.csv", "holdings log CSV (every event, holding after each)")
	year := fs.Int("year", 0, "show K4 sales only for this income year")
	var securityFlags, depaFlags, nordnetFiles multiFlag
	fs.Var(&securityFlags, "security", "only this security, name or ISIN (repeatable); default: all in the export")
	fs.Var(&depaFlags, "depa", "only this Nordnet account (Depå, repeatable); default: all. Leave out ISK/KF accounts")
	fs.Var(&nordnetFiles, "nordnet", "Nordnet export file (repeatable); default: Nordnet*.csv in the folder")
	costsFlag := fs.String("costs", "", "cost basis file for deposits; default: "+costPattern+" in the folder")
	decimalPoint := fs.Bool("decimal-point", false, "write CSV with '.' decimals and ',' separator")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: nordnetskv [flags] [directory]\n\n"+
			"Builds SKV 2198 tables and K4 summaries from Nordnet exports.\n\n")
		fs.PrintDefaults()
	}
	flagArgs, pos := splitArgs(os.Args[1:], fs)
	fs.Parse(flagArgs)
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}

	nnPaths := []string(nordnetFiles)
	if len(nnPaths) == 0 {
		nnPaths = find(dir, "Nordnet*.csv")
	}
	if len(nnPaths) == 0 {
		fatal("Nordnet*.csv hittades inte i %s", dir)
	}
	secs, known := loadNordnet(nnPaths, depaFlags, securityFlags)
	if len(secs) == 0 {
		fatal("Inga värdepapperstransaktioner i Nordnet-filerna.")
	}

	costPath := *costsFlag
	if costPath == "" {
		if hits := find(dir, costPattern); len(hits) > 0 {
			costPath = hits[0]
		}
	}
	var costs []*CostRow
	if costPath != "" {
		costs = loadCosts(costPath)
	} else {
		any := false
		for _, s := range secs {
			for _, dp := range s.Deposits {
				any = any || !dp.Internal
			}
		}
		if any {
			tpl := filepath.Join(dir, templateName)
			writeTemplate(tpl, secs)
			fmt.Printf("Mall för omkostnadsbelopp sparad: %s\n\n", tpl)
		}
	}
	used := map[*CostRow]bool{}

	nf := numFmt{swedish: !*decimalPoint}
	var allRows []Row
	var allSales []Sale
	var allLog, summary [][]string
	for _, s := range secs {
		var mine []*CostRow
		for _, c := range costs {
			if s.matches(c.Security) {
				mine = append(mine, c)
				used[c] = true
			}
		}
		events := append(append([]Event(nil), s.Events...), depositEvents(s, mine, costPath != "")...)
		rows, sales := compute(events, s.Name)
		if len(rows) == 0 {
			continue
		}
		fmt.Printf("SKV 2198 - Genomsnittligt omkostnadsbelopp - %s\n\n", s.Name)
		var data [][]string
		for _, r := range rows {
			data = append(data, []string{r.Date.String(), r.Kind, fmtSv(r.Delta, 5), fmtSv(r.TotalQty, 5),
				fmtSv(r.Change, 2), fmtSv(r.TotalCost, 2), fmtSv(r.Avg, 2), r.Note})
		}
		printTable([]string{"Datum", "Händ.", "Antal +/-", "Totalt antal", "Inköpspris/utnyttjat",
			"Totalt omk.belopp", "Genomsn. omk.belopp", "Kommentar"}, data, map[int]bool{0: true, 1: true, 7: true})
		fmt.Println()
		allRows = append(allRows, rows...)
		allSales = append(allSales, sales...)
		allLog = append(allLog, buildLog(s, rows, sales, nf)...)
		last := rows[len(rows)-1]
		summary = append(summary, []string{s.Name, fmtSv(last.TotalQty, 5), fmtSv(s.Closing, 5), fmtSv(last.Avg, 2)})
		if math.Abs(s.Closing-last.TotalQty) > 0.01 {
			warn("%s: beräknat innehav %.5f st skiljer sig från Nordnets %.5f st (%s).",
				s.Name, last.TotalQty, s.Closing, s.ClosingDate)
		}
	}
	for _, c := range costs {
		if !used[c] && !known[strings.ToUpper(c.Security)] {
			warn("Kostnadsfilen: '%s' %s finns inte bland värdepappren i Nordnet-exporten - ignoreras.", c.Security, c.Date)
		}
	}

	var csvRows [][]string
	for _, r := range allRows {
		csvRows = append(csvRows, []string{r.Name, r.Date.String(), r.Kind, nf.f(r.Delta, 5), nf.f(r.TotalQty, 5),
			nf.f(r.Change, 2), nf.f(r.TotalCost, 2), nf.f(r.Avg, 2), r.Note})
	}
	if len(csvRows) > 0 {
		writeCSV(*output, []string{"Beteckning", "Datum", "Händelse", "Antal +/-", "Totalt antal",
			"Inköpspris (+) / utnyttjat omk.belopp (-), kr", "Totalt outnyttjat omk.belopp, kr",
			"Genomsnittligt omk.belopp, kr", "Kommentar"}, csvRows, nf.swedish)
		fmt.Printf("SKV 2198-tabeller sparade: %s\n", *output)
	}

	// K4
	var shown []Sale
	for _, s := range allSales {
		if *year == 0 || s.Date.Year() == *year {
			shown = append(shown, s)
		}
	}
	if len(shown) > 0 {
		sort.SliceStable(shown, func(i, j int) bool {
			a, b := shown[i], shown[j]
			if a.Date.Year4() != b.Date.Year4() {
				return a.Date.Year4() < b.Date.Year4()
			}
			if x, y := strings.ToUpper(a.Name), strings.ToUpper(b.Name); x != y {
				return x < y
			}
			return a.Date.Before(b.Date)
		})
		var years []string
		for _, s := range shown {
			if y := s.Date.Year4(); len(years) == 0 || years[len(years)-1] != y {
				years = append(years, y)
			}
		}
		var k4, disp [][]string
		pos := func(v float64) float64 { return math.Max(v, 0) }
		fmt.Println("\nK4 avsnitt A - försäljningar")
		for _, y := range years {
			var tp, to, gain, loss float64
			for _, s := range shown {
				if s.Date.Year4() != y {
					continue
				}
				k4 = append(k4, []string{y, s.Date.String(), nf.f(s.Qty, 5), s.Name, nf.f(s.Proceeds, 0),
					nf.f(s.Cost, 0), nf.f(pos(s.Result), 0), nf.f(pos(-s.Result), 0), s.Ref})
				disp = append(disp, []string{y, s.Date.String(), s.Name, fmtSv(s.Qty, 0), fmtSv(s.Proceeds, 0),
					fmtSv(s.Cost, 0), fmtSv(pos(s.Result), 0), fmtSv(pos(-s.Result), 0)})
				tp += s.Proceeds
				to += s.Cost
				gain += pos(s.Result)
				loss += pos(-s.Result)
			}
			k4 = append(k4, []string{y, "SUMMA", "", "", nf.f(tp, 0), nf.f(to, 0), nf.f(gain, 0), nf.f(loss, 0), ""})
			disp = append(disp, []string{y, "SUMMA K4", "", "", fmtSv(tp, 0), fmtSv(to, 0), fmtSv(gain, 0), fmtSv(loss, 0)})
			disp = append(disp, make([]string, 8))
		}
		printTable([]string{"År", "Datum", "Värdepapper", "Antal", "Försäljningspris", "Omkostnadsbelopp",
			"Vinst", "Förlust"}, disp[:len(disp)-1], map[int]bool{0: true, 1: true, 2: true})
		writeCSV(*k4Path, []string{"Inkomstår", "Datum", "Antal", "Beteckning", "Försäljningspris",
			"Omkostnadsbelopp", "Vinst", "Förlust", "Nota"}, k4, nf.swedish)
		fmt.Printf("\nK4-underlag sparat: %s  (avrunda till hela kronor på blanketten)\n", *k4Path)
	}

	// Holdings log
	var logCSV [][]string
	for _, r := range allLog {
		if *year == 0 || r[0] == strconv.Itoa(*year) {
			logCSV = append(logCSV, r)
		}
	}
	if len(logCSV) > 0 {
		writeCSV(*k4HoldPath, logHeader, logCSV, nf.swedish)
		fmt.Printf("Innehavslogg (alla händelser, innehav efter varje) sparad: %s\n", *k4HoldPath)
	}

	fmt.Println("\nSammanfattning")
	printTable([]string{"Värdepapper", "Innehav beräknat", "Innehav Nordnet", "Genomsn. omk.belopp"},
		summary, map[int]bool{0: true})

	if len(warnings) > 0 {
		fmt.Println("\nVarningar / kontrollera:")
		for _, m := range warnings {
			fmt.Println("  * " + m)
		}
	}
}
