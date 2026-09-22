package translator

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// builtEpisode is an episode assembled from remote scenario data, with line
// order preserved (dialogue flow).
type builtEpisode struct {
	episodeNo             string
	scenarioID            string
	scenarioCanonicalJSON string
	scenarioSHA256        string
	title                 string
	sourceTitle           string
	talkKeys              []string
	talkData              map[string]string
	speakerNames          map[string]string
	lines                 []store.OrderedLine
}

type eventStorySyncOutcome struct {
	Processed            int
	PartialErrors        []error
	AITranslationSkipped int
	AITranslationNote    string
}

// toOrdered converts built episodes (keyed by episode no) into the ordered
// slice the EventStore import expects, sorted by numeric episode number.
func toOrderedEpisodes(eps map[string]builtEpisode, lineSource string) []store.OrderedEpisode {
	nos := make([]string, 0, len(eps))
	for no := range eps {
		nos = append(nos, no)
	}
	sort.Slice(nos, func(i, j int) bool { return atoiSafe(nos[i]) < atoiSafe(nos[j]) })
	out := make([]store.OrderedEpisode, 0, len(nos))
	for _, no := range nos {
		ep := eps[no]
		sources := make(map[string]string, len(ep.talkKeys))
		for _, jp := range ep.talkKeys {
			sources[jp] = lineSource
		}
		out = append(out, store.OrderedEpisode{
			EpisodeNo: ep.episodeNo, ScenarioID: ep.scenarioID,
			ScenarioCanonicalJSON: ep.scenarioCanonicalJSON, ScenarioSHA256: ep.scenarioSHA256,
			Title: ep.title, TitleSource: lineSource, SourceTitle: ep.sourceTitle,
			TalkKeys: ep.talkKeys, TalkData: ep.talkData, TalkSources: sources,
			SpeakerNames: ep.speakerNames, Lines: ep.lines,
		})
	}
	return out
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// syncEventStoriesCNOnly mirrors the legacy strategy: walk JP stories from the
// first not-yet-official event, write official CN where available, and on a
// 3-event empty streak fall back to JP-pending + auto LLM for newer events.
func (t *Translator) syncEventStoriesCNOnly(progressCurrent, progressTotal int) (eventStorySyncOutcome, error) {
	outcome := eventStorySyncOutcome{}
	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		return outcome, err
	}
	cnStories, err := t.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		return outcome, err
	}
	cnEvents, err := t.fetchMasterdata("events.json", "cn")
	if err != nil {
		return outcome, err
	}
	cnStoryByEvent := byIntID(cnStories, "eventId")
	cnEventSet := map[int]bool{}
	for _, e := range cnEvents {
		cnEventSet[getInt(e, "id")] = true
	}
	sort.Slice(jpStories, func(i, j int) bool {
		return getInt(jpStories[i], "eventId") < getInt(jpStories[j], "eventId")
	})

	states, localMax, err := t.eventStore.EventSyncStates()
	if err != nil {
		return outcome, err
	}
	t.backfillMissingEventScenarios(jpStories, states, &outcome, progressCurrent, progressTotal)
	latestOfficialCN, firstLLM := 0, 0
	for _, st := range states {
		if st.IsOfficialCN && st.EventID > latestOfficialCN {
			latestOfficialCN = st.EventID
		}
		if st.IsLLM && (firstLLM == 0 || st.EventID < firstLLM) {
			firstLLM = st.EventID
		}
	}
	startCN := 1
	if firstLLM > 0 {
		startCN = firstLLM
	} else if latestOfficialCN > 0 {
		startCN = latestOfficialCN + 1
	}
	// A scenario fetch failure leaves an event unimported below the cursor while
	// later events advance it. Rewind to the lowest CN-side event that is neither
	// imported as official CN nor locally preserved so this run retries it.
	frontier := startCN
	for _, jpStory := range jpStories {
		eventID := getInt(jpStory, "eventId")
		if eventID >= startCN {
			break
		}
		if !cnEventSet[eventID] || cnStoryByEvent[eventID] == nil {
			continue
		}
		if st, ok := states[eventID]; ok && (st.IsOfficialCN || st.PreserveLocal) {
			continue
		}
		startCN = eventID
		break
	}

	emptyStreak := 0
	stoppedByEmpty := false
	lastChecked := 0

	for _, jpStory := range jpStories {
		if err := t.runContext().Err(); err != nil {
			return outcome, err
		}
		eventID := getInt(jpStory, "eventId")
		if eventID < startCN {
			continue
		}
		lastChecked = eventID

		if st, ok := states[eventID]; ok && (st.IsOfficialCN || st.PreserveLocal) {
			emptyStreak = 0
			continue
		}
		if !cnEventSet[eventID] || cnStoryByEvent[eventID] == nil {
			emptyStreak++
			if emptyStreak >= 3 {
				stoppedByEmpty = true
				break
			}
			continue
		}

		t.setNote(fmt.Sprintf("cn-sync event story %d", eventID))
		t.emit("sync.progress", fmt.Sprintf("正在更新活动剧情 Event #%d", eventID), progressCurrent, progressTotal)
		episodes, hasTalk, hasTitleOnly, episodeErrors := t.buildOfficialCNEpisodes(jpStory, cnStoryByEvent[eventID])
		if len(episodeErrors) > 0 {
			for _, episodeErr := range episodeErrors {
				wrapped := fmt.Errorf("event %d: %w", eventID, episodeErr)
				outcome.PartialErrors = append(outcome.PartialErrors, wrapped)
				log.Printf("[translate] event story partial failure: %v", wrapped)
			}
			continue // scenario fetch failed; retry next round
		}
		if !hasTalk {
			// Official CN titles without talk text mean CN has started publishing
			// this event, so it must not count toward the empty streak. Events
			// below the pre-rewind frontier are retries of old gaps; only the
			// frontier itself signals where CN publication currently ends.
			if !hasTitleOnly && eventID >= frontier {
				emptyStreak++
				if emptyStreak >= 3 {
					stoppedByEmpty = true
					break
				}
			}
			continue
		}
		emptyStreak = 0

		meta := model.EventStoryMeta{Source: "official_cn", Version: "1.0", LastUpdated: time.Now().Unix()}
		runCtx := t.runContext()
		if err := runCtx.Err(); err != nil {
			return outcome, err
		}
		imported, err := t.eventStore.ImportOrderedForSyncContext(runCtx, eventID, meta, toOrderedEpisodes(episodes, "cn"))
		if err != nil {
			return outcome, err
		}
		if !imported {
			continue
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "official_cn", IsOfficialCN: true}
		if eventID > localMax {
			localMax = eventID
		}
		outcome.Processed++
	}

	if stoppedByEmpty {
		fallbackStart := localMax + 1
		log.Printf("[translate] event stories: CN empty streak at event %d, JP-pending fallback from %d", lastChecked, fallbackStart)
		fallbackOutcome, err := t.fillEventStoriesJPPending(jpStories, fallbackStart, states, progressCurrent, progressTotal)
		if err != nil {
			return outcome, err
		}
		outcome.Processed += fallbackOutcome.Processed
		outcome.PartialErrors = append(outcome.PartialErrors, fallbackOutcome.PartialErrors...)
		outcome.AITranslationSkipped += fallbackOutcome.AITranslationSkipped
		outcome.AITranslationNote = fallbackOutcome.AITranslationNote
	}
	return outcome, nil
}

// buildOfficialCNEpisodes fetches JP + CN scenarios and pairs JP text to CN
// translation by position. Returns (episodes, hasTalkData, hasTitleOnly, errors).
func (t *Translator) buildOfficialCNEpisodes(jpStory, cnStory map[string]any) (map[string]builtEpisode, bool, bool, []error) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes, err := validatedEventStoryEpisodes(jpStory, true, nil)
	if err != nil {
		return nil, false, false, []error{err}
	}
	cnEpisodes, err := validatedEventStoryEpisodes(cnStory, false, nil)
	if err != nil {
		return nil, false, false, []error{err}
	}
	cnByEp := byIntID(cnEpisodes, "episodeNo")

	episodes := map[string]builtEpisode{}
	hasTalk, hasTitleOnly := false, false
	var errs []error
	type fetchResult struct {
		ep         map[string]any
		epNo       int
		scenarioID string
		jpScenario any
		cnScenario any
		canonical  string
		sha256     string
		err        error
	}
	jobs := make(chan map[string]any)
	results := make(chan fetchResult, len(jpEpisodes))
	workers := t.fetchConcurrency()
	if workers > len(jpEpisodes) {
		workers = len(jpEpisodes)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ep := range jobs {
				epNo := getInt(ep, "episodeNo")
				scenarioID := getString(ep, "scenarioId")
				if scenarioID == "" {
					results <- fetchResult{ep: ep, epNo: epNo, err: fmt.Errorf("missing scenarioId")}
					continue
				}
				scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
				jpScenario, err := t.fetchJPScenarioJSON(scenarioPath)
				if err != nil {
					results <- fetchResult{ep: ep, epNo: epNo, scenarioID: scenarioID, err: err}
					continue
				}
				canonical, digest, err := store.CanonicalizeEventScenario(jpScenario, scenarioID)
				if err != nil {
					results <- fetchResult{ep: ep, epNo: epNo, scenarioID: scenarioID, err: err}
					continue
				}
				cnScenario, err := t.fetchCNScenarioJSON(scenarioPath)
				if err == nil {
					err = validateScenarioTalkData(cnScenario)
				}
				results <- fetchResult{
					ep: ep, epNo: epNo, scenarioID: scenarioID,
					jpScenario: jpScenario, cnScenario: cnScenario, canonical: canonical, sha256: digest, err: err,
				}
			}
		}()
	}
	go func() {
		for _, ep := range jpEpisodes {
			jobs <- ep
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	for fetched := range results {
		if fetched.err != nil {
			errs = append(errs, fmt.Errorf("episode %d (%s): %w", fetched.epNo, fetched.scenarioID, fetched.err))
			continue
		}
		ep, epNo, scenarioID := fetched.ep, fetched.epNo, fetched.scenarioID
		jpScenario, cnScenario := fetched.jpScenario, fetched.cnScenario

		jpTalkRaw, _ := asMap(jpScenario)["TalkData"].([]any)
		cnTalkRaw, _ := asMap(cnScenario)["TalkData"].([]any)
		if len(jpTalkRaw) != len(cnTalkRaw) {
			errs = append(errs, fmt.Errorf("episode %d (%s): JP/CN TalkData length mismatch (%d != %d)",
				epNo, scenarioID, len(jpTalkRaw), len(cnTalkRaw)))
			continue
		}
		jpTalk := toMapSlice(jpTalkRaw)
		cnTalk := toMapSlice(cnTalkRaw)
		if len(jpTalk) != len(jpTalkRaw) || len(cnTalk) != len(cnTalkRaw) {
			errs = append(errs, fmt.Errorf("episode %d (%s): TalkData entries must be objects", epNo, scenarioID))
			continue
		}
		talkData := map[string]string{}
		speakerNames := map[string]string{}
		var talkOrder []string
		var episodeLines []store.OrderedLine
		seen := map[string]bool{}
		translated := false
		for i := 0; i < len(jpTalk); i++ {
			var cnLine map[string]any
			if i < len(cnTalk) {
				cnLine = cnTalk[i]
			}
			jpBody := strings.TrimSpace(getString(jpTalk[i], "Body"))
			cnBody := strings.TrimSpace(getString(cnLine, "Body"))
			cnSpeaker := strings.TrimSpace(getString(cnLine, "WindowDisplayName"))
			if jpBody != "" {
				text := ""
				if cnBody != "" && jpBody != cnBody {
					text = cnBody
				}
				line := store.OrderedLine{
					JPKey: jpBody, Text: text, Source: "cn", SpeakerName: cnSpeaker,
					ScenarioPosition: i * 2, Field: "body",
				}
				episodeLines = append(episodeLines, line)
				// Untranslated JP lines keep their place in the key order with
				// empty text, so the legacy rows cover the whole episode and stay
				// visible as AI gap-fill targets.
				if !seen[jpBody] {
					talkOrder = append(talkOrder, jpBody)
					seen[jpBody] = true
					talkData[jpBody] = ""
				}
				if text != "" {
					talkData[jpBody] = text
					translated = true
					if cnSpeaker != "" {
						speakerNames[jpBody] = cnSpeaker
					}
				}
			}
			jpName := strings.TrimSpace(getString(jpTalk[i], "WindowDisplayName"))
			cnName := strings.TrimSpace(getString(cnLine, "WindowDisplayName"))
			if jpName != "" {
				text := ""
				if cnName != "" && jpName != cnName {
					text = cnName
				}
				episodeLines = append(episodeLines, store.OrderedLine{
					JPKey: jpName, Text: text, Source: "cn", ScenarioPosition: i*2 + 1, Field: "speaker",
				})
				if !seen[jpName] {
					talkOrder = append(talkOrder, jpName)
					seen[jpName] = true
					talkData[jpName] = ""
				}
				if text != "" {
					talkData[jpName] = text
					translated = true
				}
			}
		}

		cnTitle := strings.TrimSpace(getString(cnByEp[epNo], "title"))
		if cnTitle == strings.TrimSpace(getString(ep, "title")) {
			cnTitle = ""
		}
		if translated {
			hasTalk = true
		} else if cnTitle != "" {
			hasTitleOnly = true
		}
		episodes[strconv.Itoa(epNo)] = builtEpisode{
			episodeNo: strconv.Itoa(epNo), scenarioID: scenarioID,
			scenarioCanonicalJSON: fetched.canonical, scenarioSHA256: fetched.sha256,
			title: cnTitle, sourceTitle: strings.TrimSpace(getString(ep, "title")),
			talkKeys: talkOrder, talkData: talkData, speakerNames: speakerNames, lines: episodeLines,
		}
	}
	return episodes, hasTalk, hasTitleOnly, errs
}

// buildJPPendingEpisodes fetches JP-only scenario text (no CN), leaving cn empty.
func (t *Translator) buildJPPendingEpisodes(jpStory map[string]any) (map[string]builtEpisode, []error) {
	return t.buildSelectedJPPendingEpisodes(jpStory, nil)
}

func (t *Translator) buildSelectedJPPendingEpisodes(jpStory map[string]any, selected map[string]bool) (map[string]builtEpisode, []error) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes, err := validatedEventStoryEpisodes(jpStory, true, selected)
	if err != nil {
		return nil, []error{err}
	}
	episodes := map[string]builtEpisode{}
	var errs []error
	type fetchResult struct {
		ep         map[string]any
		epNo       int
		scenarioID string
		jpScenario any
		canonical  string
		sha256     string
		err        error
	}
	jobs := make(chan map[string]any)
	results := make(chan fetchResult, len(jpEpisodes))
	workers := t.fetchConcurrency()
	if workers > len(jpEpisodes) {
		workers = len(jpEpisodes)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ep := range jobs {
				epNo := getInt(ep, "episodeNo")
				scenarioID := getString(ep, "scenarioId")
				if scenarioID == "" {
					results <- fetchResult{ep: ep, epNo: epNo, err: fmt.Errorf("missing scenarioId")}
					continue
				}
				scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
				jpScenario, err := t.fetchJPScenarioJSON(scenarioPath)
				canonical, digest := "", ""
				if err == nil {
					canonical, digest, err = store.CanonicalizeEventScenario(jpScenario, scenarioID)
				}
				results <- fetchResult{ep: ep, epNo: epNo, scenarioID: scenarioID,
					jpScenario: jpScenario, canonical: canonical, sha256: digest, err: err}
			}
		}()
	}
	go func() {
		for _, ep := range jpEpisodes {
			jobs <- ep
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	for fetched := range results {
		ep, epNo, scenarioID := fetched.ep, fetched.epNo, fetched.scenarioID
		title := strings.TrimSpace(getString(ep, "title"))
		if fetched.err != nil {
			errs = append(errs, fmt.Errorf("episode %d (%s): %w", epNo, scenarioID, fetched.err))
			continue
		}
		jpScenario := fetched.jpScenario
		jpTalk := toMapSlice(asMap(jpScenario)["TalkData"])
		talkData := map[string]string{}
		speakerNames := map[string]string{}
		var talkOrder []string
		var episodeLines []store.OrderedLine
		seen := map[string]bool{}
		for scenarioIndex, talk := range jpTalk {
			jpBody := strings.TrimSpace(getString(talk, "Body"))
			jpSpeaker := strings.TrimSpace(getString(talk, "WindowDisplayName"))
			if jpBody != "" {
				episodeLines = append(episodeLines, store.OrderedLine{
					JPKey: jpBody, Text: "", Source: "jp_pending", SpeakerName: jpSpeaker,
					ScenarioPosition: scenarioIndex * 2, Field: "body",
				})
				talkData[jpBody] = ""
				if !seen[jpBody] {
					talkOrder = append(talkOrder, jpBody)
					seen[jpBody] = true
				}
				if jpSpeaker != "" {
					speakerNames[jpBody] = jpSpeaker
				}
			}
			if jpSpeaker != "" {
				episodeLines = append(episodeLines, store.OrderedLine{
					JPKey: jpSpeaker, Text: "", Source: "jp_pending",
					ScenarioPosition: scenarioIndex*2 + 1, Field: "speaker",
				})
				talkData[jpSpeaker] = ""
				if !seen[jpSpeaker] {
					talkOrder = append(talkOrder, jpSpeaker)
					seen[jpSpeaker] = true
				}
			}
		}
		episodes[strconv.Itoa(epNo)] = builtEpisode{
			episodeNo: strconv.Itoa(epNo), scenarioID: scenarioID,
			scenarioCanonicalJSON: fetched.canonical, scenarioSHA256: fetched.sha256,
			title: title, sourceTitle: title, talkKeys: talkOrder, talkData: talkData,
			speakerNames: speakerNames, lines: episodeLines,
		}
	}
	return episodes, errs
}

func validatedEventStoryEpisodes(story map[string]any, requireScenarioID bool, selected map[string]bool) ([]map[string]any, error) {
	raw, ok := story["eventStoryEpisodes"].([]any)
	if !ok {
		return nil, fmt.Errorf("eventStoryEpisodes must be an array")
	}
	parsed := toMapSlice(raw)
	if selected == nil && len(parsed) != len(raw) {
		return nil, fmt.Errorf("eventStoryEpisodes entries must all be objects")
	}

	maxInt := int64(^uint(0) >> 1)
	episodes := make([]map[string]any, 0, len(parsed))
	seenEpisodes := map[int]bool{}
	seenScenarios := map[string]bool{}
	for index, episode := range parsed {
		episodeNo, valid := positiveEventEpisodeNo(episode["episodeNo"], maxInt)
		if selected != nil && (!valid || !selected[strconv.Itoa(episodeNo)]) {
			continue
		}
		if !valid {
			return nil, fmt.Errorf("eventStoryEpisodes[%d].episodeNo must be a positive integer", index)
		}
		if seenEpisodes[episodeNo] {
			return nil, fmt.Errorf("eventStoryEpisodes has duplicate episodeNo %d", episodeNo)
		}
		scenarioID := getString(episode, "scenarioId")
		if requireScenarioID {
			if strings.TrimSpace(scenarioID) == "" {
				return nil, fmt.Errorf("eventStoryEpisodes[%d].scenarioId must be nonempty", index)
			}
			if seenScenarios[scenarioID] {
				return nil, fmt.Errorf("eventStoryEpisodes has duplicate scenarioId %q", scenarioID)
			}
			seenScenarios[scenarioID] = true
		}
		seenEpisodes[episodeNo] = true
		episodes = append(episodes, episode)
	}
	if selected != nil {
		for episodeNo, wanted := range selected {
			if !wanted {
				continue
			}
			parsedNo, err := strconv.Atoi(episodeNo)
			if err != nil || parsedNo <= 0 || !seenEpisodes[parsedNo] {
				return nil, fmt.Errorf("selected episode %q is missing from eventStoryEpisodes", episodeNo)
			}
		}
	}
	return episodes, nil
}

func positiveEventEpisodeNo(value any, maxInt int64) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number <= 0 || number > float64(maxInt) {
			return 0, false
		}
		parsed := int(number)
		return parsed, float64(parsed) == number
	case int:
		return number, number > 0
	case int64:
		if number <= 0 || number > maxInt {
			return 0, false
		}
		return int(number), true
	case string:
		parsed, err := strconv.Atoi(number)
		return parsed, err == nil && parsed > 0
	default:
		return 0, false
	}
}

// fillEventStoriesJPPending writes JP-pending stories for new events and runs
// auto LLM translation on them.
func (t *Translator) fillEventStoriesJPPending(jpStories []map[string]any, startEventID int, states map[int]store.EventSyncState, progressCurrent, progressTotal int) (eventStorySyncOutcome, error) {
	outcome := eventStorySyncOutcome{}
	skipAI := false
	if reason, unavailable := t.automaticLLMUnavailable(); unavailable {
		skipAI = true
		outcome.AITranslationNote = reason
	}
	for _, jpStory := range jpStories {
		if err := t.runContext().Err(); err != nil {
			return outcome, err
		}
		eventID := getInt(jpStory, "eventId")
		if eventID < startEventID {
			continue
		}
		if _, exists := states[eventID]; exists {
			continue
		}
		t.setNote(fmt.Sprintf("cn-sync JP-pending event story %d", eventID))
		t.emit("sync.progress", fmt.Sprintf("正在拉取 JP 剧情 Event #%d", eventID), progressCurrent, progressTotal)
		episodes, episodeErrors := t.buildJPPendingEpisodes(jpStory)
		for _, episodeErr := range episodeErrors {
			wrapped := fmt.Errorf("event %d: %w", eventID, episodeErr)
			outcome.PartialErrors = append(outcome.PartialErrors, wrapped)
			log.Printf("[translate] event story partial failure: %v", wrapped)
		}
		if len(episodeErrors) > 0 {
			continue
		}
		if len(episodes) == 0 {
			continue
		}
		meta := model.EventStoryMeta{Source: "jp_pending", Version: "1.0", LastUpdated: time.Now().Unix()}
		runCtx := t.runContext()
		if err := runCtx.Err(); err != nil {
			return outcome, err
		}
		imported, err := t.eventStore.ImportOrderedForSyncContext(runCtx, eventID, meta, toOrderedEpisodes(episodes, "unknown"))
		if err != nil {
			return outcome, err
		}
		if !imported {
			continue
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "jp_pending"}
		outcome.Processed++
		if skipAI {
			outcome.AITranslationSkipped++
			continue
		}
		// Auto-translate the freshly written JP-pending story. Failure is
		// optional: it does not discard imported data or fail the CN sync. Once
		// unavailable, skip AI for the rest of this run and leave JP pending.
		if translated, err := t.autoTranslateEventStory(eventID); err != nil {
			skipAI = true
			outcome.AITranslationSkipped++
			outcome.AITranslationNote = fmt.Sprintf("Event #%d 自动 AI 翻译在保存 %d 条后暂停：%v", eventID, translated, err)
			log.Printf("[translate] %s", outcome.AITranslationNote)
			t.emit("sync.progress", fmt.Sprintf("Event #%d 已保存，LLM 不可用，已跳过后续 AI", eventID), progressCurrent, progressTotal)
		}
	}
	return outcome, nil
}

func validateScenarioTalkData(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("scenario root must be an object")
	}
	if _, ok := object["TalkData"].([]any); !ok {
		return fmt.Errorf("scenario TalkData must be an array")
	}
	return nil
}

func (t *Translator) backfillMissingEventScenarios(jpStories []map[string]any, states map[int]store.EventSyncState,
	outcome *eventStorySyncOutcome, progressCurrent, progressTotal int) {
	for _, jpStory := range jpStories {
		if t.runContext().Err() != nil {
			return
		}
		eventID := getInt(jpStory, "eventId")
		state, ok := states[eventID]
		if !ok || !state.MissingScenarios {
			continue
		}
		missing, err := t.eventStore.MissingScenarioEpisodes(eventID)
		if err != nil {
			outcome.PartialErrors = append(outcome.PartialErrors, fmt.Errorf("event %d scenario coverage: %w", eventID, err))
			continue
		}
		if len(missing) == 0 {
			state.MissingScenarios = false
			states[eventID] = state
			continue
		}
		t.setNote(fmt.Sprintf("backfill event scenario %d", eventID))
		t.emit("sync.progress", fmt.Sprintf("正在补全活动剧情原始场景 Event #%d", eventID), progressCurrent, progressTotal)
		missingEpisodes := make(map[string]bool, len(missing))
		for _, identity := range missing {
			missingEpisodes[identity.EpisodeNo] = true
		}
		episodes, episodeErrors := t.buildSelectedJPPendingEpisodes(jpStory, missingEpisodes)
		if len(episodeErrors) > 0 {
			for _, episodeErr := range episodeErrors {
				wrapped := fmt.Errorf("event %d scenario backfill: %w", eventID, episodeErr)
				outcome.PartialErrors = append(outcome.PartialErrors, wrapped)
				log.Printf("[translate] event scenario backfill failure: %v", wrapped)
			}
			continue
		}
		selected := make(map[string]builtEpisode, len(missing))
		valid := true
		for _, identity := range missing {
			episode, exists := episodes[identity.EpisodeNo]
			if !exists || episode.scenarioID != identity.ScenarioID {
				outcome.PartialErrors = append(outcome.PartialErrors,
					fmt.Errorf("event %d episode %s scenario identity changed", eventID, identity.EpisodeNo))
				valid = false
				break
			}
			selected[identity.EpisodeNo] = episode
		}
		if !valid {
			continue
		}
		if err := t.runContext().Err(); err != nil {
			outcome.PartialErrors = append(outcome.PartialErrors, err)
			return
		}
		if err := t.eventStore.BackfillScenarios(eventID, toOrderedEpisodes(selected, "unknown")); err != nil {
			outcome.PartialErrors = append(outcome.PartialErrors, fmt.Errorf("event %d scenario backfill: %w", eventID, err))
			continue
		}
		remaining, err := t.eventStore.MissingScenarioEpisodes(eventID)
		if err != nil || len(remaining) != 0 {
			if err == nil {
				err = fmt.Errorf("%d episodes remain incomplete", len(remaining))
			}
			outcome.PartialErrors = append(outcome.PartialErrors, fmt.Errorf("event %d scenario coverage: %w", eventID, err))
			continue
		}
		state.MissingScenarios = false
		states[eventID] = state
	}
}
