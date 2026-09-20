package ipp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// A printer is a row: the label in its URL, the principal it is granted to,
// and the name a client shows for it. `txco://ipp/printer` writes the row;
// the `ipp` personality reads it on every request, before it reads a
// credential. A label with no active row does not exist.
//
// The grant is one principal per printer. Widening it ("these people may
// also print here") is a change to what this row carries and to the two
// places the head compares a principal: the login and Send-Document.

// Printer statuses.
const (
	PrinterActive   = "active"
	PrinterDisabled = "disabled"
)

// MaxDisplayName is the longest display name a printer keeps, in bytes: the
// DNS-SD instance-name limit, because the name is also `printer-dns-sd-name`.
const MaxDisplayName = 63

// ErrPrinterNotFound is returned when no printer matches.
var ErrPrinterNotFound = errors.New("ipp: printer not found")

// printerLabel is what names a printer in the path: a DNS-label-ish name,
// lowercase. Every valid label is also a valid scope instance, so a
// credential can name one printer (`ipp:front-desk:print`).
var printerLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// ValidLabel reports whether s can name a printer. The head applies it to
// the URL and the op to its `printer` param, so a row no URL reaches cannot
// be registered.
func ValidLabel(s string) bool { return printerLabel.MatchString(s) }

// Printer is one registered printer.
type Printer struct {
	Tenant      string
	Label       string
	DisplayName string // "" = show the label
	PrincipalID string // `<kind>:<name>`: who may print here
	Status      string
	CreatedBy   string // the base stack that registered it
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Active reports whether the printer accepts requests.
func (p Printer) Active() bool { return p.Status == PrinterActive }

// Name is what a client shows for the printer: its display name, else its
// label.
func (p Printer) Name() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Label
}

const printerCols = `tenant, label, display_name, principal_id, status, created_by, created_at, updated_at`

func scanPrinter(row scanner) (Printer, error) {
	var p Printer
	var created, updated string
	if err := row.Scan(&p.Tenant, &p.Label, &p.DisplayName, &p.PrincipalID, &p.Status,
		&p.CreatedBy, &created, &updated); err != nil {
		return Printer{}, err
	}
	p.CreatedAt = parseTS(created)
	p.UpdatedAt = parseTS(updated)
	return p, nil
}

// ClipDisplayName cuts a display name to MaxDisplayName bytes, on a rune
// boundary.
func ClipDisplayName(s string) string {
	if len(s) <= MaxDisplayName {
		return s
	}
	cut := MaxDisplayName
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// GetPrinter returns one printer, whatever its status.
func (s *Store) GetPrinter(ctx context.Context, tenant, label string) (Printer, error) {
	p, err := scanPrinter(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+printerCols+` FROM ipp_printers WHERE tenant = ? AND label = ?`), tenant, label))
	if errors.Is(err, sql.ErrNoRows) {
		return Printer{}, ErrPrinterNotFound
	}
	if err != nil {
		return Printer{}, fmt.Errorf("ipp: get printer: %w", err)
	}
	return p, nil
}

// UpsertPrinter registers a printer, or changes the one that has its label.
// A "" DisplayName or Status keeps what an existing row has (a new row
// defaults to no display name, and active); CreatedBy is set on insert only.
// It returns the row as stored, and whether this call created it.
func (s *Store) UpsertPrinter(ctx context.Context, p Printer) (Printer, bool, error) {
	if p.Tenant == "" || !ValidLabel(p.Label) || p.PrincipalID == "" {
		return Printer{}, false, errors.New("ipp: upsert printer: tenant, a valid label and a principal are required")
	}
	switch p.Status {
	case "", PrinterActive, PrinterDisabled:
	default:
		return Printer{}, false, fmt.Errorf("ipp: upsert printer: status %q", p.Status)
	}
	p.DisplayName = ClipDisplayName(p.DisplayName)

	_, err := s.GetPrinter(ctx, p.Tenant, p.Label)
	created := errors.Is(err, ErrPrinterNotFound)
	if err != nil && !created {
		return Printer{}, false, err
	}

	// One statement, so two nodes registering the same label agree: the row
	// exists afterwards either way, and `created` is only advisory.
	now := s.ts()
	insertStatus := p.Status
	if insertStatus == "" {
		insertStatus = PrinterActive
	}
	if _, err := s.db.ExecContext(ctx, s.rb(
		`INSERT INTO ipp_printers (tenant, label, display_name, principal_id, status, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant, label) DO UPDATE SET
		     principal_id = excluded.principal_id,
		     display_name = CASE WHEN ? = '' THEN ipp_printers.display_name ELSE excluded.display_name END,
		     status       = CASE WHEN ? = '' THEN ipp_printers.status ELSE excluded.status END,
		     updated_at   = excluded.updated_at`),
		p.Tenant, p.Label, p.DisplayName, p.PrincipalID, insertStatus, p.CreatedBy, now, now,
		p.DisplayName, p.Status); err != nil {
		return Printer{}, false, fmt.Errorf("ipp: upsert printer: %w", err)
	}
	out, err := s.GetPrinter(ctx, p.Tenant, p.Label)
	if err != nil {
		return Printer{}, false, err
	}
	return out, created, nil
}

// DeletePrinter removes a printer. It reports whether a row was there. Its
// jobs stay: a job already committed is still delivered.
func (s *Store) DeletePrinter(ctx context.Context, tenant, label string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rb(
		`DELETE FROM ipp_printers WHERE tenant = ? AND label = ?`), tenant, label)
	if err != nil {
		return false, fmt.Errorf("ipp: delete printer: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListPrinters returns a tenant's printers, by label.
func (s *Store) ListPrinters(ctx context.Context, tenant string) ([]Printer, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT `+printerCols+` FROM ipp_printers WHERE tenant = ? ORDER BY label`), tenant)
	if err != nil {
		return nil, fmt.Errorf("ipp: list printers: %w", err)
	}
	defer rows.Close()
	var out []Printer
	for rows.Next() {
		p, err := scanPrinter(rows)
		if err != nil {
			return nil, fmt.Errorf("ipp: list printers: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
