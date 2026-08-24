# First seed candidates (hand-picked 2026-08-24)

Hand-run archaeology over FreegleDockerWSL master (2,285 non-merge commits since
June); filter: <=3 files, <=120 lines, commit includes test changes (so every one is
agentic-gradable fail-to-pass), all August 2026 = post-cutoff for every current
model. Feed these shas to `eval seed-history` first, before any bulk sweep.

## Go track (backend-api)
- 5aa795a42 fix(membership): apply the pagination cursor when searching members
  (membership.go + test; clean single-concern fix, mid rung)
- 4954294b7 fix(aiimage): send only the params Flux Schnell accepts (aiimage.go +
  unit test; well-specified external-API constraint, mid rung)
- 330fa4bf0 fix(ripple): do not exact-test a post a ring has already admitted
  (rippling domain; challenging rung)

## Vue track (frontend-ui)
- 2879df734 fix(groups): fetch community names when the selector has none
  (GroupSelect.vue + spec). KNOWN CLAUDE TRIP-UP: the blank-names-on-cold-load bug
  was misdiagnosed as a postcode issue first. Challenging; session history exists
  for brief derivation.
- f256f5b80 fix(browse): do not re-ask the same search when the feed reloads
  (PostMap.vue + spec). KNOWN CLAUDE TRIP-UP: first fix attempt (PR #1376) targeted
  the wrong surface entirely. Challenging; session history exists.
- 7c58c5afd fix(moderation): anchor Approved Messages to the approved copy
  (ModMessage + ModMessages + spec; 3 files, cross-component reasoning, challenging)

## PHP track (batch / database)
- fa2adc22d fix(illustrations): stop the batch re-running a query it cannot make
  progress on (service + unit test; simple-to-mid rung)
- ec90ba284 fix(digest): mail a member once about a post, not once per shared group
  (UnifiedDigestService + test; dedup semantics, challenging)

## Test-writing nature (single test file changed; graded single-turn, not
fail-to-pass, since there is no held-out fix)
- c443339c3 test(rippling): assert the ring's extent, not which corner MySQL lists
  first (Go)
- 8c280b800 fix(mail): update chat_messages by chatid (only the test file changed
  in this commit; classify as test-writing)

## Rung-1 sanity floor
Pick mechanically at seed time: single-file <=10 line commits without later fix-ups,
e.g. 2d947cfe9 (10L test tweak), a83719a52 (1L e2e allowance). The harvest's clean
sampling covers the rest.

Why these first: two are DOCUMENTED model trip-ups from our own history (exactly the
library's founding purpose), the rest spread the three main tracks across mid and
challenging rungs, and every non-test-writing entry carries its own held-out tests
for agentic grading.
