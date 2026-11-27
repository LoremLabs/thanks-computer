// Package home is the one folder `txco attach` keeps on a machine:
//
//	<txco home>/attach/
//	  credentials/<id>.json   one per attached workspace: the address, the
//	                          credential's id, its folder and its key (0600)
//	  credentials/<id>.lock   held by the txco attach serving a while-running one
//	  OPS/_attach/…           the owner's own rules for the _attach stack
//	  state/<id>/             TXCO_ATTACH_STATE: a product's own files
//	  jobs/<job>/             detached jobs (chassis/workspace/detach)
//	  chassis/                the small chassis's own stores
//	  attach.log
//
// It is a package of its own, with no chassis in it, because three things
// read it: the command, the link, and the workspace provider that runs the
// owner's commands.
package home

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Home is the attach folder.
type Home struct{ Dir string }

func (h Home) CredentialsDir() string { return filepath.Join(h.Dir, "credentials") }
func (h Home) JobsDir() string        { return filepath.Join(h.Dir, "jobs") }
func (h Home) ChassisDir() string     { return filepath.Join(h.Dir, "chassis") }
func (h Home) LogPath() string        { return filepath.Join(h.Dir, "attach.log") }

// OpsParent is the directory that holds the owner's OPS/ tree: what the
// chassis is given as --system-opstacks-dir.
func (h Home) OpsParent() string { return h.Dir }

// StateDir is one credential's own folder for a product's files.
func (h Home) StateDir(credential string) string {
	return filepath.Join(h.Dir, "state", credential)
}

// Credential is one attached workspace, as this machine holds it.
type Credential struct {
	ID string `json:"credential"`
	// Address is the chassis to dial, scheme and host.
	Address string `json:"address"`
	// Name and Label are what the chassis said the credential is for.
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"`
	// Root is the folder commands run in: the absolute path of the
	// directory `txco attach <code>` was run in. It is written once, at
	// enrollment, and nothing later changes it — not a reconnect, not a
	// restart, not running the command from somewhere else. Moving a
	// workspace to another folder is a new code, enrolled there.
	Root string `json:"root"`
	// Seed is the Ed25519 private key's seed. It never leaves this file.
	Seed      []byte    `json:"key"`
	Machine   string    `json:"machine,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Ended is set when the chassis said this credential is finished
	// (replaced or revoked). It is kept, not deleted, so the owner can see
	// what happened; it is never dialed again.
	Ended string `json:"ended,omitempty"`
	// WhileRunning: the share ends when the `txco attach` that made it
	// stops, as the site asked. Only that process serves it, holding its
	// lock (Hold) all along, and it removes the credential as it stops. One
	// whose lock is free was left by a process that died: the next `txco
	// attach` removes it.
	WhileRunning bool `json:"while_running,omitempty"`
}

// Key is the credential's private key.
func (c Credential) Key() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(c.Seed) }

var idRE = regexp.MustCompile(`^atc-[0-9a-f]{32}$`)

// ValidID reports whether id has a credential id's shape, so an id can
// never name a path outside the folder.
func ValidID(id string) bool { return idRE.MatchString(id) }

func (c Credential) check() error {
	switch {
	case !ValidID(c.ID):
		return fmt.Errorf("not a credential id: %q", c.ID)
	case c.Address == "":
		return errors.New("credential has no address")
	case !filepath.IsAbs(c.Root):
		return fmt.Errorf("credential root %q is not an absolute path", c.Root)
	case len(c.Seed) != ed25519.SeedSize:
		return errors.New("credential has no key")
	}
	return nil
}

// Save writes a credential, readable by its owner alone.
func (h Home) Save(c Credential) error {
	if err := c.check(); err != nil {
		return err
	}
	dir := h.CredentialsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+c.ID+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(raw, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(dir, c.ID+".json"))
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// Get reads one credential. ok is false when there is none, or its file
// does not hold a usable one.
func (h Home) Get(id string) (Credential, bool) {
	if !ValidID(id) {
		return Credential{}, false
	}
	raw, err := os.ReadFile(filepath.Join(h.CredentialsDir(), id+".json"))
	if err != nil {
		return Credential{}, false
	}
	var c Credential
	if json.Unmarshal(raw, &c) != nil || c.ID != id || c.check() != nil {
		return Credential{}, false
	}
	return c, true
}

// Load reads every credential in the folder, ended ones included, in id
// order.
func (h Home) Load() ([]Credential, error) {
	ents, err := os.ReadDir(h.CredentialsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Credential
	for _, e := range ents {
		id, isJSON := strings.CutSuffix(e.Name(), ".json")
		if !isJSON {
			continue
		}
		if c, ok := h.Get(id); ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// End marks a credential as finished, with the chassis's reason.
func (h Home) End(id, reason string) error {
	c, ok := h.Get(id)
	if !ok {
		return nil
	}
	c.Ended = reason
	return h.Save(c)
}

// The locks this process holds, by path. A lock is an flock on
// credentials/<id>.lock: liveness is read by trying it, never from a pid.
var (
	heldMu sync.Mutex
	held   = map[string]*os.File{}
)

func (h Home) lockPath(id string) string { return filepath.Join(h.CredentialsDir(), id+".lock") }

// Hold takes id's lock for this process, until Release or Remove. It fails
// while any other holder has it, in this process or another.
func (h Home) Hold(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("not a credential id: %q", id)
	}
	if err := os.MkdirAll(h.CredentialsDir(), 0o700); err != nil {
		return err
	}
	p := h.lockPath(id)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return errors.New("another txco attach holds it")
	}
	heldMu.Lock()
	held[p] = f
	heldMu.Unlock()
	return nil
}

// Holds reports whether this process holds id's lock.
func (h Home) Holds(id string) bool {
	heldMu.Lock()
	defer heldMu.Unlock()
	return held[h.lockPath(id)] != nil
}

// Release gives up id's lock, if this process holds it.
func (h Home) Release(id string) {
	p := h.lockPath(id)
	heldMu.Lock()
	f := held[p]
	delete(held, p)
	heldMu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// Left reports whether id is a while-running credential's that nobody
// holds: its txco attach died.
func (h Home) Left(id string) bool {
	if h.Holds(id) || h.Hold(id) != nil {
		return false
	}
	h.Release(id)
	return true
}

// Remove deletes a credential and its lock, and releases it. Only its
// holder removes one.
func (h Home) Remove(id string) error {
	if !ValidID(id) {
		return nil
	}
	err := os.Remove(filepath.Join(h.CredentialsDir(), id+".json"))
	if os.IsNotExist(err) {
		err = nil
	}
	_ = os.Remove(h.lockPath(id))
	h.Release(id)
	return err
}

// Pretty shows a path with the home directory as "~", as it is shown to
// the owner and to the chassis.
func Pretty(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}
