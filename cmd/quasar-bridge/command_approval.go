package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/commandprompt"
)

const codeLifetime = 90 * time.Second
const issueInterval = 30 * time.Second

var errCommandApprovalRequired = errors.New("spoken approval required")
var errAudioUnavailable = errors.New("Колонка не смогла произнести код. Проверьте локальный аудиовыход и попробуйте снова.")
var errLimitsUnavailable = errors.New("Не удалось прочитать или сохранить лимит попыток на колонке.")
var errExpiredChallenge = errors.New("Подтверждение больше недоступно. Вернитесь к команде и запросите новый код.")
var errWrongCode = errors.New("Код не совпал. Введите число, которое произнесла колонка.")

type approvalWait struct{ after time.Duration }

func (e approvalWait) Error() string {
	if e.after >= time.Hour {
		return fmt.Sprintf("Лимит попыток исчерпан. Попробуйте через %d ч.", (e.after+time.Hour-1)/time.Hour)
	}
	return fmt.Sprintf("Запросить код можно через %d с.", (e.after+time.Second-1)/time.Second)
}

func writeApprovalError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if wait, ok := errors.AsType[approvalWait](err); ok {
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.FormatInt(int64((wait.after+time.Second-1)/time.Second), 10))
	} else if errors.Is(err, errExpiredChallenge) {
		status = http.StatusGone
	} else if errors.Is(err, errWrongCode) {
		status = http.StatusUnprocessableEntity
	}
	writeDiagnosticError(w, status, err.Error())
}

type commandTicket struct {
	Status    string `json:"status"`
	ID        string `json:"challenge_id"`
	ExpiresIn int    `json:"expires_in"`
}

type commandChallenge struct {
	id      string
	expires time.Time
	session string
	number  int
	edit    commandEdit
	ready   bool
}

func (p *commandChallenge) ticket(now time.Time) commandTicket {
	return commandTicket{Status: "confirmation_required", ID: p.id, ExpiresIn: int((p.expires.Sub(now) + time.Second - 1) / time.Second)}
}

// Both panel listeners share a single audible challenge. The number and its
// exact edit live only in memory. A service restart must not reset guess limits.
type commandApprovals struct {
	mu      sync.Mutex
	pending *commandChallenge
	now     func() time.Time
	speak   func(context.Context, int) error
}

func (a *commandApprovals) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

type commandLimits struct {
	NextIssue time.Time `json:"next_issue"`
	Failures  int       `json:"failures"`
	WindowEnd time.Time `json:"window_end"`
}

func readCommandLimits(path string, now time.Time) (commandLimits, error) {
	var l commandLimits
	body, err := os.ReadFile(path + ".command-limits")
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil || json.Unmarshal(body, &l, json.RejectUnknownMembers(true)) != nil || l.Failures < 0 || l.Failures > 3 {
		return l, errLimitsUnavailable
	}
	if !now.Before(l.WindowEnd) {
		l.Failures = 0
	}
	return l, nil
}

func (l commandLimits) save(path string) error {
	data, err := json.Marshal(l)
	if err != nil || writeConfiguration(path+".command-limits", data, 0600) != nil {
		return errLimitsUnavailable
	}
	return nil
}

func (a *commandApprovals) reserve(path, session string, edit commandEdit) (p *commandChallenge, fresh bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	if p := a.pending; p != nil && now.Before(p.expires) {
		if p.ready && p.session == session && reflect.DeepEqual(p.edit, edit) {
			return p, false, nil // Retried request with a lost HTTP response.
		}
		return nil, false, approvalWait{p.expires.Sub(now)}
	}
	limits, err := readCommandLimits(path, now)
	if err != nil {
		return nil, false, err
	}
	if now.Before(limits.NextIssue) {
		return nil, false, approvalWait{limits.NextIssue.Sub(now)}
	}
	number, err := rand.Int(rand.Reader, big.NewInt(900))
	if err != nil {
		return nil, false, errAudioUnavailable
	}
	limits.NextIssue = now.Add(issueInterval)
	if err := limits.save(path); err != nil {
		return nil, false, err
	}
	p = &commandChallenge{id: rand.Text(), expires: now.Add(codeLifetime), session: session, number: 100 + int(number.Int64()), edit: edit}
	a.pending = p
	return p, true, nil
}

func (a *commandApprovals) begin(ctx context.Context, path, session string, edit commandEdit) (commandTicket, error) {
	p, fresh, err := a.reserve(path, session, edit)
	if err != nil {
		return commandTicket{}, err
	}
	if !fresh {
		return p.ticket(a.clock()), nil
	}
	speak := a.speak
	if speak == nil {
		speak = commandprompt.Play
	}
	playCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = speak(playCtx, p.number)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil || playCtx.Err() != nil || a.pending != p {
		if a.pending == p {
			a.pending = nil
		}
		return commandTicket{}, errAudioUnavailable
	}
	p.ready = true
	return p.ticket(a.clock()), nil
}

func (a *commandApprovals) finish(path, session, id, code string, cancel bool) (commandEdit, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()
	p := a.pending
	if p == nil || !p.ready || !now.Before(p.expires) || p.session != session || p.id != id {
		return commandEdit{}, errExpiredChallenge
	}
	if cancel {
		a.pending = nil
		return commandEdit{}, nil
	}
	limits, err := readCommandLimits(path, now)
	if err != nil {
		a.pending = nil
		return commandEdit{}, err
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(strconv.Itoa(p.number))) != 1 {
		if limits.Failures == 0 {
			limits.WindowEnd = now.Add(24 * time.Hour)
		}
		limits.Failures++
		if limits.Failures >= 3 {
			a.pending = nil
			limits.NextIssue = now.Add(24 * time.Hour)
			limits.WindowEnd = limits.NextIssue
		}
		if err := limits.save(path); err != nil {
			a.pending = nil
			return commandEdit{}, err
		}
		if limits.Failures >= 3 {
			return commandEdit{}, approvalWait{limits.NextIssue.Sub(now)}
		}
		return commandEdit{}, errWrongCode
	}
	a.pending = nil
	// Only physical confirmation lets the next legitimate edit speak immediately.
	// Keep unsuccessful guesses in the station-wide daily budget.
	limits.NextIssue = time.Time{}
	if err := limits.save(path); err != nil {
		return commandEdit{}, err
	}
	return p.edit, nil
}
