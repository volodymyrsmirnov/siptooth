# Siptooth — Execution Report (2026-10-14)

Plan: docs/superpowers/plans/2026-10-14-siptooth.md. Spec: docs/superpowers/specs/2026-10-14-siptooth-design.md (rev 4).
Executed task by task with Sonnet 5.5 implementers and reviewers; Codex gpt-6.1-sol / gpt-6-astra reviews of the plan, each milestone and the final tree; Fable 5.1 final review.

Final gates: make check, full race suite (x2), bridge race x5, Go 1.24.9 vet/build/test, linux/arm64 build — all green.

## Rulings, parked items and deferred minors (from the execution ledger)

- Ruling: work directly on main (no worktree) — new local repo without remote; owner's established practice (allochka: commit per task on main, no push) and plan Global Constraints say so — cost if wrong: rebase onto a branch later, trivial.
- Ruling: implementers Sonnet 5.5 (owner instruction); task reviewers Sonnet 5.5; Codex gpt-6.1-sol / gpt-6-astra milestone reviews after Tasks 8, 11, 16, 20 and a plan review before Task 2 — cost if wrong: weaker per-task review on subtle SIP/concurrency tasks, mitigated by Codex milestones.
- Task 1: minor (deferred): pi-hardware-test uses `ssh -o BatchMode=yes` but settings.json allow rules lack that form (permission prompts) — surface to owner, not auto-changed
- Task 1: minor (deferred): Makefile $(BINARY) target depends on go.sum before deps exist
- Ruling: host unit keeps allochka's template (ExecStart <exe> run --config <cfg>) instead of the brief's literal %h paths — install supplies real paths, run reads env itself — cost if wrong: small unit tweak in Task 18
- Ruling: Task 2 (pure copy) reviewed by controller mechanical diff against allochka sources instead of a reviewer subagent — the diff shows only renames + one new test — cost if wrong: copy defects surface in later tasks' tests
- Task 3-4: minor (deferred, carried to Task 5): ExpandDisplayName can't reject CR/LF/NUL — config validation must reject control chars in phone.number, anonymous_user and all templates
- Task 3-4: minor (deferred): no test for placeholder in @-less template; CleanCaller keeps interior '+'; unknown Codec treated as PCMU
- Ruling: gpt-6.1-sol plan review (19 findings) applied as binding "Amendments" blocks in Tasks 6,8,9,10,11,12,14,15,16,17,18,19 — key decisions: RTP session owns its socket reader (STUN via session); sip.Call/Attempt are interfaces; INVITE built manually with transport+destination then WriteInvite; Abandon publishes result immediately; remote BYE handling moved to Task 10; TransportLost = dialog keepalive write failure; display name outer quotes stripped for sipgo; Prepare async after expansion; hfp ErrResponse/ErrNotSent classification; hooks runner 8 KiB + full env minus password; registration owns idle keepalive; stun_failed rate-limited in adapter; sip test isolated UA — cost if wrong: rework in those tasks
- Ruling: forked second 2xx handled only if sipgo exposes it; otherwise documented limitation — forking not expected from Zadarma/ElevenLabs — cost if wrong: a stray far-end leg on a forking trunk until its own timeout
- Ruling: race tests run via `make race` (cgo allowed for that target only) and a Go 1.24 toolchain vet/test at the final gate — cost if wrong: none
- Task 5: minor (deferred): header validation order non-deterministic (sort keys); anonymous_user branching convoluted; TestLookupPasswordFeedsValidate weak/ignores WriteFile error; port boundary 1024/65534 untested
- Task 6: minor (deferred): STUN accepts 0.0.0.0/port 0 mapped address (should reject); lenient padding; no IPv6/garble fuzz test; no concurrent Decide test
- Task 7: minor (deferred): Marshal doesn't validate Addr.Is4/LocalKey length (caller owns)
- Task 12: minor (deferred): dispatcher Run drops queued events at shutdown (final call.ended outcome=shutdown may be lost — daemon must let the dispatcher drain briefly); post-reap group kill pgid reuse window
- Task 13: review → Needs fixes (transient RTP send error kills call; cadence/no-double-send not asserted); Ruling: transient RTP send errors tolerated up to 5 s, net.ErrClosed fatal — spec 4.5 'fatal' — cost if wrong: 5 s of one-way audio before teardown; fix round 1 dispatched (FIX_BASE 7188b70)
- Task 13: minor (deferred): pump logs via slog.Default (no logger param); SetStartup-once test indirect
- Ruling: dial timer starts when Prepare starts (not at INVITE) — bounds total ring-forwarding time incl. a hung Prepare — cost if wrong: INVITE gets slightly less than dial_timeout
- Ruling: dialing STUN keepalive owned by the Task 17 adapter (tied to Prepare ctx / until Media.Start) — interfaces unchanged — cost if wrong: none
- Ruling: Tasks 15+16 dispatched to one implementer (two commits) — both rewrite coordinator.go/states.go; avoids ramp-up and conflicts — cost if wrong: larger single review
- Task 10: parked — OnState !byeSeen → TransportLost branch untested — Ruling: no deterministic trigger in sipgo; keepalive TransportLost path is tested; media_timeout backstops — cost if wrong: a mislabelled end cause in logs/hooks
- Task 10: complete (commits 096aa98..eda7f83, 1 parked)
- Task 11: minor (deferred, combine with gpt-6.1-sol sip review fixes): register cycle publishes unregistered Contact after followup cap; no floor on granted/2 refresh; Unregister `sent` set late; Register twice orphans; re-INVITE 200 Contact may be stale; retransmitted re-INVITE same branch → 500?; registration tests timing-sensitive
- Task 17: review → Needs fixes (UDP local_port 0 binds ephemeral but Contact says 5060; rtp_bind 0.0.0.0 not rejected; minors). Ruling: daemon maps sip.local_port 0 → 5060 for UDP (spec 5.5); sip test passes 0 explicitly; adapter advertises the UA's actual bound port (new sip accessor) — queued until the SIP fix round lands (ua.go conflict)
- Tasks 15-16: parked — an HFP AT write already blocked in the RFCOMM transport cannot be cancelled — Ruling: bounded by the 3 s write deadline; cancelling mid-write could corrupt the AT stream; requires a stalled RFCOMM link plus a call boundary inside 3 s — cost if wrong: a stale ATA/CHUP reaching the phone in that degenerate case
- Tasks 15-16: complete (commits 67da1f8..f1ac270, 1 parked)
- SIP: fix round 2 dispatched (strict-router destination, fork BYE after recovered ACK, UA.Close hang on auth retry); spec §5.6 corrected by controller; findings 5 (TLS hostname not pinned) and 11 (TLS handshake socket to GC) parked as accepted limitations — cost if wrong: rare flow mismatch with multi-A-record TLS hosts; leaked fd per failed handshake until GC
- Task 19: fix round 1/5 (2 important + 3 minor addressed; commit 1bfb25c) — controller-checked; minor (deferred): options line merges "no answer" and "no rport"
- Final fix A: 3ae18d3, 6cfee06 (OutgoingSetup event, late window survives setup clear, shutdown resolves ATA, PhoneBusy/CleanupPending power-cycle gate, bounded BT stop, fake BT in tests). Ruling: hfp also emits CallSetupCleared when an outgoing setup returns to 0 with call 0 — needed so PhoneBusy clears; incoming/outgoing setups cannot overlap — cost if wrong: a spurious caller_abandoned in an impossible overlap
- FINAL: parked — shutdown while ATA pending can discard a CallActive / attribute a new ring's CallActive to the old ATA (answering.go ~372/~397) — Ruling: requires `systemctl stop` exactly during an answer race; fix after M2 — cost if wrong: a stopped daemon may leave an answered call silent or CHUP the owner's next ring during that shutdown
- FINAL: parked — PhoneBusy not seeded from initial CIND at attach (coordinator.go ~317) — Ruling: only matters if the daemon starts while a call is in progress and the call lasts until the hourly power cycle; fix after M2 — cost if wrong: power cycle drops BT audio of that call (phone falls back to handset)
- FINAL: parked — TCP/TLS Contact still guesses 5060 if the TLS handshake exceeds the 1 s OPTIONS budget (sipadapter.go ~269, register.go ~294) — Ruling: qualified providers handshake well under 1 s; M1 sip test shows contact source — cost if wrong: far-end BYE relies on connection reuse / media timeout
- FINAL: parked — implicit IPv6 route produces IPv4-labelled SDP (sipadapter.go ~208) — Ruling: both providers resolve IPv4 and sipgo prefers IPv4 — cost if wrong: no audio on an IPv6-only path
