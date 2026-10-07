package adkrunner

// pgxSessionService is a session.Service over the app pgx pool, storing
// rows in exactly the format ADK's database service writes: plain JSON in
// jsonb columns (nil state maps as '{}', never NULL), raw bytes in bytea,
// "" as NULL only in the five nullable-text columns. Same tables
// (migration 041 owns the DDL), same queries, same error contract --
// which is what lets rows written by either implementation be read by
// the other, proven by TestLiveRowCompat, and what lets ADK's own
// sessiontestsuite judge this implementation like any other.
//
// Why this exists instead of reusing ADK's gorm service: the app pool
// speaks pgx natively, and routing the hot conversation path through an
// ORM plus a stdlib bridge only to reach the same tables costs two
// dependencies (gorm, postgres driver) for zero behavior. The day ADK
// changes its storage format, the conformance suite -- not production
// traffic -- is where we find out.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// pgxSessionService implements session.Service directly on pgx.
type pgxSessionService struct {
	pool *pgxpool.Pool
}

// NewPgxSessionService builds the pgx-backed session store on the app
// pool. No extra connections: every query runs on the shared pool.
func NewPgxSessionService(pool *pgxpool.Pool) session.Service {
	return &pgxSessionService{pool: pool}
}

// pgxLocalSession mirrors ADK's localSession: in-memory view with the
// same append/trim/state rules, so behavior matches before SQL runs.
type pgxLocalSession struct {
	appName   string
	userID    string
	sessionID string

	mu        sync.RWMutex
	events    []*session.Event
	state     map[string]any
	updatedAt time.Time
}

func (s *pgxLocalSession) ID() string      { return s.sessionID }
func (s *pgxLocalSession) AppName() string { return s.appName }
func (s *pgxLocalSession) UserID() string  { return s.userID }
func (s *pgxLocalSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}

func (s *pgxLocalSession) State() session.State {
	return &pgxState{mu: &s.mu, state: s.state}
}

func (s *pgxLocalSession) Events() session.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*session.Event, len(s.events))
	copy(out, s.events)
	return pgxEvents(out)
}

func (s *pgxLocalSession) appendEvent(event *session.Event) error {
	if event.Partial {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Actions.StateDelta == nil {
		// Nothing to do -- but the event itself is still appended below.
	} else {
		if s.state == nil {
			s.state = make(map[string]any)
		}
		maps.Copy(s.state, event.Actions.StateDelta)
	}
	s.events = append(s.events, trimTempEventState(event))
	return nil
}

type pgxEvents []*session.Event

func (e pgxEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, event := range e {
			if !yield(event) {
				return
			}
		}
	}
}

func (e pgxEvents) Len() int { return len(e) }

func (e pgxEvents) At(i int) *session.Event {
	if i >= 0 && i < len(e) {
		return e[i]
	}
	return nil
}

type pgxState struct {
	mu    *sync.RWMutex
	state map[string]any
}

func (s *pgxState) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.state[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return val, nil
}

func (s *pgxState) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[key] = value
	return nil
}

func (s *pgxState) All() iter.Seq2[string, any] {
	s.mu.RLock()
	cp := maps.Clone(s.state)
	s.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for k, v := range cp {
			if !yield(k, v) {
				return
			}
		}
	}
}

// --- encoding: the exact storage contract ---

// encodeStateMap serializes a state map the way ADK's stateMap does: nil
// becomes '{}', never SQL NULL.
func encodeStateMap(m map[string]any) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeStateMap(src any) (map[string]any, error) {
	if src == nil {
		return make(map[string]any), nil
	}
	// pgx decodes jsonb straight into Go values when the target is any.
	if m, ok := src.(map[string]any); ok {
		if m == nil {
			return make(map[string]any), nil
		}
		return m, nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return nil, fmt.Errorf("failed to unmarshal JSON value: %T", src)
	}
	if len(b) == 0 || string(b) == "null" {
		return make(map[string]any), nil
	}
	out := make(map[string]any)
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeDynamicJSON mirrors dynamicJSON: empty is SQL NULL.
func encodeDynamicJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON for database column: %s", string(raw))
	}
	return string(raw), nil
}

func encodeActions(a session.EventActions) ([]byte, error) {
	return json.Marshal(a)
}

func decodeActions(b []byte) (session.EventActions, error) {
	var out session.EventActions
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	return out, nil
}

// nullString stores "" as SQL NULL, matching the five nullable-text
// columns (branch, isolation_scope, error_code, error_message is on the
// response not the row... see mapping below).
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefOrZero(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- state scope helpers (same rules as ADK: app:/user:/temp:) ---

func extractStateDeltas(delta map[string]any) (app, user, sess map[string]any) {
	if len(delta) == 0 {
		return nil, nil, nil
	}
	app = make(map[string]any)
	user = make(map[string]any)
	sess = make(map[string]any)
	for key, value := range delta {
		if clean, ok := strings.CutPrefix(key, session.KeyPrefixApp); ok {
			app[clean] = value
		} else if clean, ok := strings.CutPrefix(key, session.KeyPrefixUser); ok {
			user[clean] = value
		} else if !strings.HasPrefix(key, session.KeyPrefixTemp) {
			sess[key] = value
		}
	}
	return app, user, sess
}

func mergeStates(app, user, sess map[string]any) map[string]any {
	out := make(map[string]any, len(app)+len(user)+len(sess))
	maps.Copy(out, sess)
	for k, v := range app {
		out[session.KeyPrefixApp+k] = v
	}
	for k, v := range user {
		out[session.KeyPrefixUser+k] = v
	}
	return out
}

func trimTempEventState(event *session.Event) *session.Event {
	if len(event.Actions.StateDelta) == 0 {
		return event
	}
	filtered := make(map[string]any)
	for key, value := range event.Actions.StateDelta {
		if !strings.HasPrefix(key, session.KeyPrefixTemp) {
			filtered[key] = value
		}
	}
	if len(filtered) == len(event.Actions.StateDelta) {
		return event
	}
	cp := *event
	cp.Actions.StateDelta = filtered
	return &cp
}

// --- row mapping ---

const eventColumns = `id, app_name, user_id, session_id, invocation_id, author, actions,
	long_running_tool_ids_json, routes_json, output_json, node_info_json, requested_input_json,
	branch, isolation_scope, "timestamp", content, grounding_metadata, custom_metadata,
	usage_metadata, citation_metadata, input_transcription, output_transcription,
	partial, turn_complete, error_code, error_message, interrupted`

// eventInsertSQL is the precomputed event INSERT: 27 placeholders with
// ::jsonb casts on the 12 jsonb columns (everything except actions,
// which is bytea). Built once at compile time instead of 28 Sprintf +
// a 12-entry map literal + a Join on every event.
const eventInsertSQL = `INSERT INTO events (` + eventColumns + `) VALUES (` +
	`$1,$2,$3,$4,$5,$6,$7,` +
	`$8::jsonb,$9::jsonb,$10::jsonb,$11::jsonb,$12::jsonb,` +
	`$13,$14,$15,` +
	`$16::jsonb,$17::jsonb,$18::jsonb,$19::jsonb,$20::jsonb,$21::jsonb,$22::jsonb,` +
	`$23,$24,$25,$26,$27)`

// mustJSONMap encodes maps and lists: nil or empty is NULL upstream,
// so only non-empty values are stored.
func mustJSONMap(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if string(b) == "null" || string(b) == "{}" || string(b) == "[]" {
		return nil
	}
	return b
}

// mustJSONVal encodes pointed-to structs and scalars. A nil pointer must
// encode as SQL NULL -- but passed as any, a typed nil is not == nil and
// would marshal to the four characters "null". The reflect guard catches
// exactly the Ptr/Map/Slice kinds; anything else marshals verbatim.
func mustJSONVal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if rv := reflect.ValueOf(v); (rv.Kind() == reflect.Ptr ||
		rv.Kind() == reflect.Map ||
		rv.Kind() == reflect.Slice) && rv.IsNil() {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func eventToRow(sess *pgxLocalSession, ev *session.Event) ([]any, error) {
	actions, err := encodeActions(ev.Actions)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal actions: %w", err)
	}
	encodeOpt := func(raw json.RawMessage) (any, error) {
		return encodeDynamicJSON(raw)
	}
	strList := func(ss []string) json.RawMessage {
		if len(ss) == 0 {
			return nil
		}
		b, err := json.Marshal(ss)
		if err != nil {
			return nil
		}
		return b
	}
	args := make([]any, 0, 27)
	args = append(args,
		ev.ID, sess.AppName(), sess.UserID(), sess.ID(),
		ev.InvocationID, ev.Author, actions,
	)
	for _, raw := range []json.RawMessage{
		strList(ev.LongRunningToolIDs),
		strList(ev.Routes),
		mustJSONMap(ev.Output),
		mustJSONVal(ev.NodeInfo),
		mustJSONVal(ev.RequestedInput),
	} {
		v, err := encodeOpt(raw)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	args = append(args,
		nullString(ev.Branch), nullString(ev.IsolationScope),
		ev.Timestamp,
	)
	for _, raw := range []json.RawMessage{
		mustJSONVal(ev.Content),
		mustJSONVal(ev.GroundingMetadata),
		mustJSONMap(ev.CustomMetadata),
		mustJSONVal(ev.UsageMetadata),
		mustJSONVal(ev.CitationMetadata),
		mustJSONVal(ev.InputTranscription),
		mustJSONVal(ev.OutputTranscription),
	} {
		v, err := encodeOpt(raw)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	args = append(args,
		&ev.Partial, &ev.TurnComplete,
		nullString(ev.ErrorCode), nullString(ev.ErrorMessage),
		&ev.Interrupted,
	)
	return args, nil
}

type eventRow struct {
	id, appName, userID, sessionID     string
	invocationID, author               string
	actions                            []byte
	longRunning, routes, output        json.RawMessage
	nodeInfo, requestedInput           json.RawMessage
	branch, isolationScope             *string
	timestamp                          time.Time
	content, grounding, custom         json.RawMessage
	usage, citation                    json.RawMessage
	inTrans, outTrans                  json.RawMessage
	partial, turnComplete, interrupted *bool
	errorCode, errorMessage            *string
}

func scanEventRow(row pgx.Row) (*session.Event, error) {
	var r eventRow
	var actions []byte
	var ts time.Time
	err := row.Scan(
		&r.id, &r.appName, &r.userID, &r.sessionID,
		&r.invocationID, &r.author, &actions,
		&r.longRunning, &r.routes, &r.output, &r.nodeInfo, &r.requestedInput,
		&r.branch, &r.isolationScope, &ts,
		&r.content, &r.grounding, &r.custom, &r.usage, &r.citation,
		&r.inTrans, &r.outTrans,
		&r.partial, &r.turnComplete, &r.errorCode, &r.errorMessage, &r.interrupted,
	)
	if err != nil {
		return nil, err
	}
	ev := &session.Event{}
	ev.ID = r.id
	ev.Timestamp = ts
	ev.InvocationID = r.invocationID
	ev.Author = r.author
	ev.Branch = derefOrZero(r.branch)
	ev.IsolationScope = derefOrZero(r.isolationScope)
	ev.ErrorCode = derefOrZero(r.errorCode)
	ev.ErrorMessage = derefOrZero(r.errorMessage)
	if r.partial != nil {
		ev.Partial = *r.partial
	}
	if r.turnComplete != nil {
		ev.TurnComplete = *r.turnComplete
	}
	if r.interrupted != nil {
		ev.Interrupted = *r.interrupted
	}
	if ev.Actions, err = decodeActions(actions); err != nil {
		return nil, fmt.Errorf("failed to map storage event: %w", err)
	}
	decodeList := func(raw json.RawMessage, out any) error {
		if len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
	if err := decodeList(r.longRunning, &ev.LongRunningToolIDs); err != nil {
		return nil, fmt.Errorf("failed to map storage event: %w", err)
	}
	if err := decodeList(r.routes, &ev.Routes); err != nil {
		return nil, fmt.Errorf("failed to map storage event: %w", err)
	}
	if len(r.output) > 0 {
		var v any
		if err := json.Unmarshal(r.output, &v); err != nil {
			return nil, fmt.Errorf("failed to map storage event: %w", err)
		}
		ev.Output = v
	}
	if len(r.nodeInfo) > 0 {
		var v session.NodeInfo
		if err := json.Unmarshal(r.nodeInfo, &v); err != nil {
			return nil, fmt.Errorf("failed to map storage event: %w", err)
		}
		nv := v
		ev.NodeInfo = &nv
	}
	if len(r.requestedInput) > 0 {
		var v session.RequestInput
		if err := json.Unmarshal(r.requestedInput, &v); err != nil {
			return nil, fmt.Errorf("failed to map storage event: %w", err)
		}
		rv := v
		ev.RequestedInput = &rv
	}
	return ev, evContentFrom(r, ev)
}

func evContentFrom(r eventRow, ev *session.Event) error {
	decodePtr := func(raw json.RawMessage, out any) error {
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("failed to map storage event: %w", err)
		}
		return nil
	}
	if len(r.content) > 0 {
		var v genai.Content
		if err := decodePtr(r.content, &v); err != nil {
			return err
		}
		ev.Content = &v
	}
	if len(r.grounding) > 0 {
		var v genai.GroundingMetadata
		if err := decodePtr(r.grounding, &v); err != nil {
			return err
		}
		ev.GroundingMetadata = &v
	}
	if len(r.custom) > 0 {
		var v map[string]any
		if err := decodePtr(r.custom, &v); err != nil {
			return err
		}
		ev.CustomMetadata = v
	}
	if len(r.usage) > 0 {
		var v genai.GenerateContentResponseUsageMetadata
		if err := decodePtr(r.usage, &v); err != nil {
			return err
		}
		ev.UsageMetadata = &v
	}
	if len(r.citation) > 0 {
		var v genai.CitationMetadata
		if err := decodePtr(r.citation, &v); err != nil {
			return err
		}
		ev.CitationMetadata = &v
	}
	if len(r.inTrans) > 0 {
		var v genai.Transcription
		if err := decodePtr(r.inTrans, &v); err != nil {
			return err
		}
		ev.InputTranscription = &v
	}
	if len(r.outTrans) > 0 {
		var v genai.Transcription
		if err := decodePtr(r.outTrans, &v); err != nil {
			return err
		}
		ev.OutputTranscription = &v
	}
	return nil
}

// --- session.Service ---

func (s *pgxSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req.AppName == "" || req.UserID == "" {
		return nil, fmt.Errorf("app_name and user_id are required")
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = platform.NewUUID(ctx)
	}
	stateMap := req.State
	if stateMap == nil {
		stateMap = make(map[string]any)
	}
	now := platform.Now(ctx)
	val := &pgxLocalSession{
		appName:   req.AppName,
		userID:    req.UserID,
		sessionID: sessionID,
		state:     stateMap,
		updatedAt: now,
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	appState, _, err := fetchAppStateTx(ctx, tx, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("error on create session: %w", err)
	}
	userState, _, err := fetchUserStateTx(ctx, tx, req.AppName, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("error on create session: %w", err)
	}

	appDelta, userDelta, sessionState := extractStateDeltas(req.State)
	if len(appDelta) > 0 {
		maps.Copy(appState, appDelta)
		if err := upsertAppStateTx(ctx, tx, req.AppName, appState, now); err != nil {
			return nil, fmt.Errorf("failed to save app state: %w", err)
		}
	}
	if len(userDelta) > 0 {
		maps.Copy(userState, userDelta)
		if err := upsertUserStateTx(ctx, tx, req.AppName, req.UserID, userState, now); err != nil {
			return nil, fmt.Errorf("failed to save user state: %w", err)
		}
	}
	sessionJSON, err := encodeStateMap(sessionState)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (app_name, user_id, id, state, create_time, update_time)
		 VALUES ($1,$2,$3,$4::jsonb,$5,$6)`,
		req.AppName, req.UserID, sessionID, sessionJSON, now, now); err != nil {
		return nil, fmt.Errorf("error creating session on database: %w", err)
	}

	val.state = mergeStates(appState, userState, sessionState)
	val.updatedAt = now
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	return &session.CreateResponse{Session: val}, nil
}

func (s *pgxSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return nil, fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q",
			req.AppName, req.UserID, req.SessionID)
	}
	var stateSrc any
	var createTime, updateTime time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT state, create_time, update_time FROM sessions
		  WHERE app_name=$1 AND user_id=$2 AND id=$3`,
		req.AppName, req.UserID, req.SessionID).Scan(&stateSrc, &createTime, &updateTime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q: %w", session.ErrNotFound, req.SessionID, err)
		}
		return nil, fmt.Errorf("database error while fetching session: %w", err)
	}
	storedState, err := decodeStateMap(stateSrc)
	if err != nil {
		return nil, fmt.Errorf("failed to map storage object: %w", err)
	}

	q := `SELECT ` + eventColumns + ` FROM events
	      WHERE app_name=$1 AND user_id=$2 AND session_id=$3`
	args := []any{req.AppName, req.UserID, req.SessionID}
	if !req.After.IsZero() {
		q += ` AND "timestamp" >= $4`
		args = append(args, req.After)
	}
	q += ` ORDER BY "timestamp" DESC, id DESC`
	if req.NumRecentEvents > 0 {
		q += fmt.Sprintf(` LIMIT %d`, req.NumRecentEvents)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("database error while fetching events: %w", err)
	}
	storageEvents := []*session.Event{}
	for rows.Next() {
		ev, err := scanEventRow(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to map storage event: %w", err)
		}
		storageEvents = append(storageEvents, ev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("database error while fetching events: %w", err)
	}
	rows.Close()
	for i, j := 0, len(storageEvents)-1; i < j; i, j = i+1, j-1 {
		storageEvents[i], storageEvents[j] = storageEvents[j], storageEvents[i]
	}

	appState, err := fetchAppState(ctx, s.pool, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("error on get session: %w", err)
	}
	userState, err := fetchUserState(ctx, s.pool, req.AppName, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("error on get session: %w", err)
	}
	val := &pgxLocalSession{
		appName:   req.AppName,
		userID:    req.UserID,
		sessionID: req.SessionID,
		state:     mergeStates(appState, userState, storedState),
		events:    storageEvents,
		updatedAt: updateTime,
	}
	return &session.GetResponse{Session: val}, nil
}

func (s *pgxSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req.AppName == "" {
		return nil, fmt.Errorf("app_name is required, got app_name: %q", req.AppName)
	}
	q := `SELECT app_name, user_id, id, state, update_time FROM sessions WHERE app_name=$1`
	args := []any{req.AppName}
	if req.UserID != "" {
		q += ` AND user_id=$2`
		args = append(args, req.UserID)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("database error while fetching session: %w", err)
	}
	type sessRow struct {
		app, user, id string
		state         map[string]any
		updated       time.Time
	}
	var found []sessRow
	for rows.Next() {
		var r sessRow
		var stateSrc any
		if err := rows.Scan(&r.app, &r.user, &r.id, &stateSrc, &r.updated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database error while fetching session: %w", err)
		}
		st, err := decodeStateMap(stateSrc)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to map storage object for session %s: %w", r.id, err)
		}
		r.state = st
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("database error while fetching session: %w", err)
	}
	rows.Close()

	appState, err := fetchAppState(ctx, s.pool, req.AppName)
	if err != nil {
		return nil, fmt.Errorf("error on list sessions: %w", err)
	}
	userStates := map[string]map[string]any{}
	if req.UserID != "" {
		us, err := fetchUserState(ctx, s.pool, req.AppName, req.UserID)
		if err != nil {
			return nil, fmt.Errorf("error on list sessions: %w", err)
		}
		userStates[req.UserID] = us
	} else {
		ur, err := s.pool.Query(ctx,
			`SELECT user_id, state FROM user_states WHERE app_name=$1`, req.AppName)
		if err != nil {
			return nil, fmt.Errorf("error on list sessions: %w", err)
		}
		for ur.Next() {
			var uid string
			var src any
			if err := ur.Scan(&uid, &src); err != nil {
				ur.Close()
				return nil, fmt.Errorf("error on list sessions: %w", err)
			}
			st, err := decodeStateMap(src)
			if err != nil {
				ur.Close()
				return nil, fmt.Errorf("error on list sessions: %w", err)
			}
			userStates[uid] = st
		}
		if err := ur.Err(); err != nil {
			ur.Close()
			return nil, fmt.Errorf("error on list sessions: %w", err)
		}
		ur.Close()
	}

	out := make([]session.Session, 0, len(found))
	for _, r := range found {
		us, ok := userStates[r.user]
		if !ok {
			us = make(map[string]any)
		}
		out = append(out, &pgxLocalSession{
			appName:   r.app,
			userID:    r.user,
			sessionID: r.id,
			state:     mergeStates(appState, us, r.state),
			updatedAt: r.updated,
		})
	}
	return &session.ListResponse{Sessions: out}, nil
}

func (s *pgxSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q",
			req.AppName, req.UserID, req.SessionID)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx,
		`DELETE FROM sessions WHERE app_name=$1 AND user_id=$2 AND id=$3`,
		req.AppName, req.UserID, req.SessionID); err != nil {
		return fmt.Errorf("database error during session deletion: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *pgxSessionService) AppendEvent(ctx context.Context, curSession session.Session, event *session.Event) error {
	if curSession == nil {
		return fmt.Errorf("session is nil")
	}
	if event == nil {
		return fmt.Errorf("event is nil")
	}
	if event.Partial {
		return nil
	}
	if event.ID == "" {
		event.ID = platform.NewUUID(ctx)
	}
	event.Timestamp = event.Timestamp.Truncate(time.Microsecond)

	sess, ok := curSession.(*pgxLocalSession)
	if !ok {
		return fmt.Errorf("unexpected session type %T", curSession)
	}
	if err := sess.appendEvent(event); err != nil {
		return fmt.Errorf("failed to update localSession state: %w", err)
	}
	event = trimTempEventState(event)
	if err := s.applyEvent(ctx, sess, event); err != nil {
		return err
	}
	sess.mu.Lock()
	sess.updatedAt = event.Timestamp
	sess.mu.Unlock()
	return nil
}

func (s *pgxSessionService) applyEvent(ctx context.Context, sess *pgxLocalSession, event *session.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	var storedUpdate time.Time
	var storedStateSrc []byte
	err = tx.QueryRow(ctx,
		`SELECT state, update_time FROM sessions WHERE app_name=$1 AND user_id=$2 AND id=$3`,
		sess.AppName(), sess.UserID(), sess.ID()).Scan(&storedStateSrc, &storedUpdate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %q, cannot apply event: %w", session.ErrNotFound, sess.ID(), err)
		}
		return fmt.Errorf("failed to get session: %w", err)
	}

	sess.mu.RLock()
	sessionUS := sess.updatedAt.UnixMicro()
	sess.mu.RUnlock()
	if storedUpdate.UnixMicro() > sessionUS {
		return fmt.Errorf(
			"stale session error: last update time from request (%s) is older than in database (%s)",
			time.UnixMicro(sessionUS).Format(time.RFC3339Nano),
			storedUpdate.Format(time.RFC3339Nano))
	}

	appDelta, userDelta, sessionDelta := extractStateDeltas(event.Actions.StateDelta)
	// Fetch-then-merge only when there is something to merge: a typical
	// event carries no app:/user: delta, so the old unconditional
	// fetches wasted 2 of ~7 round trips per event, every event.
	if len(appDelta) > 0 {
		appState, _, err := fetchAppStateTx(ctx, tx, sess.AppName())
		if err != nil {
			return err
		}
		maps.Copy(appState, appDelta)
		if err := upsertAppStateTx(ctx, tx, sess.AppName(), appState, event.Timestamp); err != nil {
			return fmt.Errorf("failed to save app state: %w", err)
		}
	}
	if len(userDelta) > 0 {
		userState, _, err := fetchUserStateTx(ctx, tx, sess.AppName(), sess.UserID())
		if err != nil {
			return err
		}
		maps.Copy(userState, userDelta)
		if err := upsertUserStateTx(ctx, tx, sess.AppName(), sess.UserID(), userState, event.Timestamp); err != nil {
			return fmt.Errorf("failed to save user state: %w", err)
		}
	}
	if len(sessionDelta) > 0 {
		merged, err := decodeStateMap(storedStateSrc)
		if err != nil {
			return fmt.Errorf("failed to get session: %w", err)
		}
		maps.Copy(merged, sessionDelta)
		sessionJSON, err := encodeStateMap(merged)
		if err != nil {
			return fmt.Errorf("failed to save session state: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET state=$1::jsonb, update_time=$2 WHERE app_name=$3 AND user_id=$4 AND id=$5`,
			sessionJSON, event.Timestamp, sess.AppName(), sess.UserID(), sess.ID()); err != nil {
			return fmt.Errorf("failed to save session state: %w", err)
		}
	} else if _, err := tx.Exec(ctx,
		`UPDATE sessions SET update_time=$1 WHERE app_name=$2 AND user_id=$3 AND id=$4`,
		event.Timestamp, sess.AppName(), sess.UserID(), sess.ID()); err != nil {
		// No session delta: refresh the timestamp without rewriting
		// the state jsonb. The old path re-decoded, re-encoded, and
		// rewrote the whole state on every event — a full row version
		// plus index churn for a value that did not change.
		return fmt.Errorf("failed to save session state: %w", err)
	}

	vals, err := eventToRow(sess, event)
	if err != nil {
		return fmt.Errorf("failed to map event to storage model: %w", err)
	}
	if _, err := tx.Exec(ctx, eventInsertSQL, vals...); err != nil {
		return fmt.Errorf("failed to save event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	sess.mu.Lock()
	sess.updatedAt = event.Timestamp
	sess.mu.Unlock()
	return nil
}

// --- state row helpers ---

func fetchAppState(ctx context.Context, db pgxQuerier, appName string) (map[string]any, error) {
	st, _, err := fetchAppStateTx(ctx, db, appName)
	return st, err
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func fetchAppStateTx(ctx context.Context, q pgxQuerier, appName string) (map[string]any, time.Time, error) {
	// Scan jsonb into bytes, not any: pgx decodes jsonb-to-any via
	// reflection into map[string]any (~2x slower than bytes +
	// json.Unmarshal), and every turn pays it three times.
	var src []byte
	var ut time.Time
	err := q.QueryRow(ctx,
		`SELECT state, update_time FROM app_states WHERE app_name=$1`, appName).Scan(&src, &ut)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return make(map[string]any), time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	st, err := decodeStateMap(src)
	if err != nil {
		return nil, time.Time{}, err
	}
	return st, ut, nil
}

func fetchUserState(ctx context.Context, db pgxQuerier, appName, userID string) (map[string]any, error) {
	st, _, err := fetchUserStateTx(ctx, db, appName, userID)
	return st, err
}

func fetchUserStateTx(ctx context.Context, q pgxQuerier, appName, userID string) (map[string]any, time.Time, error) {
	var src []byte
	var ut time.Time
	err := q.QueryRow(ctx,
		`SELECT state, update_time FROM user_states WHERE app_name=$1 AND user_id=$2`,
		appName, userID).Scan(&src, &ut)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return make(map[string]any), time.Time{}, nil
		}
		return nil, time.Time{}, err
	}
	st, err := decodeStateMap(src)
	if err != nil {
		return nil, time.Time{}, err
	}
	return st, ut, nil
}

func upsertAppStateTx(ctx context.Context, tx pgx.Tx, appName string, state map[string]any, ut time.Time) error {
	js, err := encodeStateMap(state)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO app_states (app_name, state, update_time) VALUES ($1,$2::jsonb,$3)
		 ON CONFLICT (app_name) DO UPDATE SET state=EXCLUDED.state, update_time=EXCLUDED.update_time`,
		appName, js, ut)
	return err
}

func upsertUserStateTx(ctx context.Context, tx pgx.Tx, appName, userID string, state map[string]any, ut time.Time) error {
	js, err := encodeStateMap(state)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO user_states (app_name, user_id, state, update_time) VALUES ($1,$2,$3::jsonb,$4)
		 ON CONFLICT (app_name, user_id) DO UPDATE SET state=EXCLUDED.state, update_time=EXCLUDED.update_time`,
		appName, userID, js, ut)
	return err
}
