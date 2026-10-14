# Issue priority triage

How an open item gets its `Priority` in the DittoFS project board. The rules are written to be
applied by a person or an agent from the item's title, body, labels and board status alone, and
their purpose is to let an agent set priorities automatically: wherever a rule would need a
judgement the text cannot support, it should be tightened rather than left to taste.

The rules cover both kinds of item on the board: repository issues and draft items. Drafts hold
what must not be public yet, security findings above all, so triage reads them like any issue but
never copies their content anywhere public: not into an issue, a comment, a commit or a PR.

Two kinds of issue are out of scope, and triage never sets or proposes their priority: sub-issues,
and issues labelled `epic`. They are how roadmap items are defined, so their priority is set by
hand.

The rules are tuned to the project's current stage: one first use case, described below, and
a lot of weight on the test suites. They will change as the use cases do. Re-tiering the board
after a change is a proposal for a person to approve, item by item, never a bulk rewrite.

## How to apply

What triage may do depends on whether the item has a priority, and whether it is pinned:

| The item | Triage |
| --- | --- |
| Has no `Priority` | Sets one with the rules below. |
| Has a `Priority`, not pinned | Leaves the field unchanged. If the rules place it elsewhere, it proposes the change to a person, who decides. |
| Is pinned | Leaves it alone and does not question it: the priority was set by hand on purpose. A pinned item with no `Priority` stays without one. |

A pin is the human override. Use it when a priority was decided for a reason the rules do not
capture, or must hold even if the rules change later: a pinned item keeps its priority through any
future revision of this file. An item is pinned when it carries the `priority-pinned` label, or
when a line of its body starts with `priority-pinned:` followed by the reason. A draft cannot carry
a label, so the body line is how a draft is pinned, and it keeps working on an issue converted
from that draft.

**Planned feature work** is new functionality that someone is building or has committed to build,
such as the implementation of a design RFC. Triage may place it with the rules below like any other
item, but its actual priority is agreed among the people doing the work, who are free to disregard
the one triage set.

Walk the tiers top-down and stop at the **first** one that matches. Write one line saying which
rule matched, so the next reader can disagree with the reasoning and not just with the label.
If no rule matches, leave the priority unset and, when possible, ask a person.

- **What the issue describes decides, not how it is phrased.** A `documentation` label on an issue
  whose real content is "a new share inherits a deleted share's files" doesn't make it a docs issue.
- **Not reproduced doesn't lower a tier.** A finding from code reading with a concrete mechanism
  ranks as if it were reproduced. Only a body that shows the mechanism cannot happen lowers it.
- **Umbrella and audit issues** (a list of findings, a tracker with sub-issues) take P2. Their
  sub-issues are out of scope like any other: a person sets their priority by hand.
- **When unsure between two tiers, take the lower one.** P0 has to stay short, or it stops
  meaning "drop everything".

## The first use case

P1 refers to it, so it is spelled out here. FSLogix profile and Office containers on an SMB share
backed by S3:

- **Clients:** Windows over SMB. Any other SMB client counts too, a Linux CIFS mount included.
  NFS is not involved.
- **Identity:** domain users from an Active Directory domain controller, authenticated over
  Kerberos or NTLM, with domain SIDs in the files' ACLs.
- **Files:** one VHDX per user, 10–30 GB or more, held open through one handle for a whole session
  of several hours.
- **Write pattern:** random, 4 KiB-aligned overwrites of data that is already uploaded. The file
  grows by writing past its end and shrinks by a truncate at sign-out.
- **Scale:** about 10,000 users, so many users, many large files, and access control between users
  matters.
- **Paths it exercises:** journal, syncer and carver under a steady stream of writes to one file,
  GC of overwritten chunks, durable handles, encryption, and installing and running the service.

## P0 — Critical

Any one of these:

1. **Data loss, corruption or unreadable data** triggered by what a client does, or by a normal
   background process (sync, GC, eviction, compaction, restart, recovery) in a supported
   configuration. What a client does includes unusual but legitimate patterns, such as a program
   that keeps writing to a file after making it read-only, or names that differ only by case across
   SMB and NFS. Corruption includes a deleted file coming back and a read that returns wrong bytes
   or zeros. Unreadable data is still stored but can no longer be read back, for example after a
   restart or an upgrade. The trigger has to be the operation, not the bytes: when the loss needs
   file content chosen to collide with an internal format, it is P1.2.
2. **Stability:** a crash, panic, deadlock or hang, an operation that fails permanently from then on,
   or resource growth that exhausts disk or memory within days of normal use.
   **Loss of availability that needs manual intervention** belongs here too: the server or a share
   stays down, or keeps failing, until an operator acts. An outage that recovers on its own is not
   P0 through this rule.
3. **A blocked shared gate:** CI on `develop`, the release build or the nightly suite cannot run, or
   reports a pass without testing anything (a false green), or becomes flaky: a test in it passes
   less than 90% of the time on unchanged code. A problem in one developer's local setup is not a
   shared gate.
4. **Security reachable from the network without credentials**: access without authentication,
   an authentication bypass, or a privilege escalation, over any protocol: SMB, NFS or the control
   plane API.
5. **Denial of service without credentials**: requests from a client that has not authenticated
   crash the server, exhaust its memory, disk or cache, or stop it serving other clients. A
   handshake that is never finished but leaves state behind counts.
6. **Denial of service through a legitimate workload**: an ordinary user, with no special rights
   and no crafted requests, stops the service for other users through the way they use it. The
   usual shape is write amplification: a mass rewrite fills the local cache or journal faster than
   backpressure slows that user down, and writes start failing or stalling for all users. Slowing
   down or stalling only the user or file that causes the load is not this rule: backpressure
   working is fine, and a defect confined to that file is placed by the other rules.
7. **A test with the wrong verdict**: a test that passes while the behaviour it checks is broken,
   or fails while it works, every time and not by chance. It hides a bug or sends people chasing
   one that does not exist, wherever it runs.

## P1 — High

Any one of these:

1. **Affects the first use case** described above. This covers SMB behaviour, large files,
   overwrites, a single file under a steady stream of writes, upload and GC paths, multi-user
   access control on SMB.
2. **Data loss or unreadable data with an unusual trigger**: either an admin action the system
   should have refused, such as moving a store that is in use, two stores on one bucket, or
   deleting a share together with its snapshots; or file content chosen to collide with an internal
   format, such as a file that starts with the compression frame marker. P0.1 is triggered by the
   operation a client performs or by a background process; here the trigger is the admin action
   or the content itself.
3. **Silent durability failure**: the system stops uploading data, or stops being able to, and
   does not report it, while nothing is lost yet. Examples are a missing bucket, a journal that
   disappeared, and a health check that stays green while uploads fail. Once data is actually lost,
   P0.1 or P1.2 places it.
4. **Storage leaks on the remote**: objects that are never reclaimed, so the bucket grows without
   limit under the first use case's overwrite pattern.
5. **Installation and first start**: a documented install path (package, Docker image, build from
   source) fails, or the server does not come up or cannot be mounted on its first start. That
   covers the docs those steps follow.
6. **Breaking configuration changes**: a config key, environment variable, CLI flag or default
   that is renamed, removed or changes meaning between releases, so an existing setup stops working
   or silently runs with different settings.
7. **Regressions**: something that worked in the previous release no longer works, unless it is P0.
8. **Security that needs an authenticated client**: a client with valid credentials can reach data
   or state it should not, such as another user's files or another client's stateids, over any
   protocol.
9. **Denial of service that needs an authenticated client**: a client with valid credentials
   sends crafted or hostile requests, such as an oversized count or a malformed structure, that
   crash the server, exhaust a resource or stop it serving others, over any protocol: SMB, NFS or
   the control plane API. A heavy but legitimate workload
   is P0.6, not this rule.
10. **Tests for the first use case**: a missing, skipped or broken test of anything the use case
   exercises, such as SMB through a Linux CIFS mount, several users on one file at once, or domain
   users against a domain controller.
11. **Non-conformance with POSIX**, whichever protocol it was found through.

## P2 — Medium

1. **Documentation**: wrong, missing or outdated docs anywhere under `docs/`, design RFCs
   included. Install and first-start
   docs that a user cannot follow to a working server are P1.5.
2. **Non-conformance with a protocol spec or a design RFC**, when no P0 or P1 rule applies. That
   covers the NFS RFCs (RFC 1813, 7530, 8881), the SMB specifications (MS-SMB2, MS-FSA, MS-DTYP)
   and the DittoFS design documents under `docs/`. SMB behaviour usually hits P1.1 first.
3. **NFS only**: NFSv3 and v4.x bugs, conformance gaps and audit findings whose effect cannot reach
   anything but NFS clients, such as protocol state, delegations or grace. A wrong POSIX result
   found through NFS is P1.11, because what it breaks is shared with SMB.
4. **Security that needs more than a client**: an attacker who can already read the bucket or log
   in to the server host, such as metadata exposed in an encrypted bucket, and hardening with no
   known way to exploit it.
5. **Test and CI gaps outside the first use case that are not a blocked gate**: a suite that
   skips, a fixture that leaks, a flaky test that does not gate a release, a runner that fails
   locally only.
6. **Investigations and spikes.**

## P3 — Low

1. **Low impact**: a wrong error code or message with no further consequence, a race on a field
   with no observable effect, goroutines that outlive shutdown, cosmetic CLI output.
2. **Feature requests nobody has planned**: integrations, and field observations that ask for
   nothing yet. Planned feature work is outside these rules.
3. **Test hygiene and refactors** with no coverage at stake.

## Calibration

Use the anchors below to check a borderline call. Each one is linked to the rule that places it.
Replace an anchor once its issue is closed.

| Issue | Tier | Rule |
| --- | --- | --- |
| #2910: sync stops short on a 160 GB file, client saw the write complete | P0 | P0.1, data loss through normal I/O |
| #2924: truncate fails permanently after file-level dedup | P0 | P0.2, an operation fails permanently |
| #3097: NFSv3 WRITE refuses the owner after `chmod 0`, the data is lost at close | P0 | P0.1, an unusual but legitimate client pattern |
| #2895: GC deletes the object before its metadata, dedup adopts missing bytes | P0 | P0.1, a background process loses data |
| #3017: carve commits starve under a steady write stream to one file | P1 | P1.1, the FSLogix write pattern |
| #3056: editing a store that is in use makes the share unreadable after restart | P1 | P1.2, an admin action that should be refused |
| #3051: uploads to a missing bucket fail silently | P1 | P1.3, silent durability failure |
| #3094: no test has two SMB users, or a domain user, on one file at once | P1 | P1.10, a test for the first use case |
| #3099: a directory's nlink reads one short right after an NFSv3 MKDIR | P1 | P1.11, POSIX, found through NFS |
| #2897: crafted content colliding with the compression magic is unreadable | P1 | P1.2, content built to collide with an internal format |
| #3078: REMOVE does not recall another client's delegation | P2 | P2.2, protocol spec (RFC 8881) |
| #3080: grace status reports 0 expected clients | P2 | P2.3, NFS |
| #2625: unlocked access to the SMB position field | P3 | P3.1, no observable effect |
