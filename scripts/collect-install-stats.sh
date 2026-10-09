#!/bin/sh
# Collect per-platform release-asset download counts into a daily time series.
#
# GitHub reports `download_count` as a LIFETIME total per asset, so a daily
# figure only exists as a difference between two snapshots. This script records
# one snapshot per run; the deltas are computed at render time (see
# docs/install-stats/index.html) rather than stored, so a backfilled or
# corrected snapshot does not leave a stale delta behind it.
#
# No retroactive history is possible: the series begins at the first run.
#
# What the numbers do and do not mean
# -----------------------------------
# Platform is NOT a usable humans-vs-CI split, and release-smoke-linux.yaml says
# so itself: it installs "exactly as a real user would (curl | sh, downloading
# from GitHub Releases)". It is deliberately indistinguishable from a Linux user
# by behaviour. Treating linux_amd64 as "CI" would therefore erase every real
# Linux user -- confidently wrong rather than merely noisy.
#
# What the smoke test IS, is countable: one job, ubuntu-latest, no matrix, so one
# successful run fetches one install's worth of assets. Hence CI subtraction:
#
#   linux humans ~= linux downloads - successful smoke runs
#
# Only `success` counts. Of 64 runs in the first sampled window, 18 were
# `skipped` and downloaded nothing, so counting runs naively overstates CI by
# ~28%. The subtraction can still land slightly negative when a run falls
# between two snapshots; the page shows that as ~0 with a footnote rather than
# hiding it, because a negative estimate is a signal the CI model needs
# recalibrating, not noise to suppress.
#
# Neither platform can separate maintainers from strangers -- at a base of ~12, a
# few contributors on a few machines each could account for all of it. Label any
# rendering accordingly; see the <h1> and the caveat block in index.html.
#
# Smoke runs are recorded per DAY, not per release: every run so far has
# head_branch=main and attributes to whichever tag was newest at run time, so
# per-release CI attribution would be approximate in a way the daily series is
# not.
#
# Usage:
#   scripts/collect-install-stats.sh [--out PATH] [--repo OWNER/NAME]
#
# Requires: gh (authenticated), jq.

set -eu

OUT="docs/install-stats/data.json"
REPO="rossoctl/cortex"

while [ $# -gt 0 ]; do
	case "$1" in
	--out)
		[ $# -ge 2 ] || { echo "--out requires a value" >&2; exit 2; }
		OUT="$2"
		shift 2
		;;
	--repo)
		[ $# -ge 2 ] || { echo "--repo requires a value" >&2; exit 2; }
		REPO="$2"
		shift 2
		;;
	-h | --help)
		sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
done

warn() { echo "warning: $*" >&2; }

command -v gh >/dev/null 2>&1 || { echo "gh is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }

today=$(date -u +%Y-%m-%d)
tmp=$(mktemp)
snap=$(mktemp)
runs=$(mktemp)
trap 'rm -f "${tmp}" "${snap}" "${runs}"' EXIT INT TERM

# --paginate to cover every release, not just the first page: lifetime totals on
# older tags still move (mirrors, stragglers) and dropping them would silently
# understate the all-time figure.
gh api "repos/${REPO}/releases" --paginate >"${tmp}"

# Successful smoke runs per day, for the CI subtraction.
#
# Queried by workflow FILE rather than by filtering every run in the repo: the
# repo-wide /actions/runs endpoint needs --paginate over thousands of runs and
# takes minutes, while this one returns the workflow's own runs in a single page.
#
# A 30-day window rather than all time: 100 runs per page covers it comfortably
# at the observed rate (~6/day), and the series only needs enough overlap to
# cover a gap between snapshots. Note this is a BOUNDED window -- once the
# series is long enough, old days keep whatever was recorded at the time.
SMOKE_WORKFLOW="release-smoke-linux.yaml"
if ! gh api \
	"repos/${REPO}/actions/workflows/${SMOKE_WORKFLOW}/runs?per_page=100" \
	--jq '[.workflow_runs[]
	       | select(.conclusion == "success")
	       | .created_at[0:10]]
	      | group_by(.)
	      | map({key: .[0], value: length})
	      | from_entries' >"${runs}" 2>/dev/null; then
	# A missing or renamed workflow must not take the whole snapshot down: the
	# download series is the primary signal and stands on its own. Record an
	# empty object, which renders as "CI unknown" rather than as "CI was zero".
	warn "could not read ${SMOKE_WORKFLOW} runs; recording no CI data for today"
	echo '{}' >"${runs}"
fi

# Per-asset counts are collapsed into per-platform sums here rather than stored
# raw: keeping every asset name would grow the file without adding signal. The
# platform is kept because it says WHERE a release is being pulled, not who by --
# it cannot separate CI from people, for the reason in the header.
#
# checksums.txt is the primary install signal, and the reason is arithmetic: the
# installer fetches TWO tarballs per install (cortex + agentop) and verifies both
# against ONE checksums file. So a platform count double-counts an install while
# checksums counts it once, and anything derived from platform counts has to be
# halved to compare with it.
#
# That 1:2 ratio is stable across every release with traffic -- v0.8.1 66:133,
# v0.8.0 10:22, v0.7.0 18:37 -- which is what makes checksums trustworthy as an
# install count rather than a coincidence. A sustained drift away from ~2.0 means
# the installer changed what it fetches, or something is pulling tarballs
# directly (v0.7.0-alpha.7 reads 3.8x, which is the latter).
#
# (An earlier version of this comment claimed checksums EQUALLED the platform sum
# on v0.8.1. That was a mid-release snapshot read as a validating identity; it is
# 1:2, not 1:1.)
jq --arg date "${today}" --slurpfile smoke "${runs}" '
  def platform_of($name):
    if   $name | test("darwin_arm64") then "darwin_arm64"
    elif $name | test("darwin_amd64") then "darwin_amd64"
    elif $name | test("linux_arm64")  then "linux_arm64"
    elif $name | test("linux_amd64")  then "linux_amd64"
    elif $name | test("^checksums")   then "checksums"
    else "other"
    end;

  {
    date: $date,
    # Successful smoke runs keyed by date. Carried on every snapshot rather than
    # only for today: the window is retrospective, so a later snapshot corrects
    # an earlier day whose runs had not all finished when it was taken.
    smoke_runs_by_day: ($smoke[0] // {}),

    # Total successful runs across the whole queried window, and the release they
    # are attributed to.
    #
    # Attribution is coarse ON PURPOSE. A run reports head_branch=main, never a
    # tag, so nothing in the API says which release it installed -- only that it
    # installed whichever one was newest at the time. Every run in the window
    # therefore gets attributed to the release that is current NOW, which is right
    # while one release stays current across the window and wrong for the rest.
    #
    # The page shows this on the row for the current release only and leaves every
    # other row blank, because blank is honest and a zero would not be. Once the
    # daily series is long enough to say which release was current on each day, CI
    # can be attributed per release properly and this field becomes redundant.
    smoke_runs_total: ($smoke[0] // {} | [.[]] | add // 0),
    # Released tags only. A draft has no public download path, so counting it
    # would add a row that can never move.
    releases: [
      .[] | select(.draft | not) | {
        tag: .tag_name,
        published_at: .published_at,
        prerelease: .prerelease,
        platforms: (
          reduce (.assets[] | {p: platform_of(.name), c: .download_count}) as $a
            ({}; .[$a.p] = ((.[$a.p] // 0) + $a.c))
        ),
      }
    ],
  }
  # Totals are derived once here so every consumer agrees on them.
  | .totals = (
      reduce (.releases[].platforms | to_entries[]) as $e
        ({}; .[$e.key] = ((.[$e.key] // 0) + $e.value))
    )
' "${tmp}" >"${snap}"

mkdir -p "$(dirname "${OUT}")"
[ -f "${OUT}" ] || echo '{"snapshots":[]}' >"${OUT}"

# Re-running on the same day replaces that day's snapshot instead of appending a
# second one. Makes the job idempotent, so a manual workflow_dispatch after a
# failed cron run is safe and leaves exactly one row per date.
merged=$(mktemp)
jq --slurpfile snap "${snap}" '
  .snapshots
  |= ( map(select(.date != $snap[0].date)) + $snap
       | sort_by(.date) )
' "${OUT}" >"${merged}"

# cat rather than mv: OUT may live on a different filesystem than TMPDIR (it
# does under the Actions worktree), where mv across devices is not atomic
# anyway, and this keeps the trap's cleanup list accurate.
cat "${merged}" >"${OUT}"
rm -f "${merged}"

days=$(jq '.snapshots | length' "${OUT}")
echo "recorded ${today} in ${OUT} (${days} day(s) of history)"
jq -r --arg d "${today}" '.snapshots[-1]
       | "  cumulative: darwin_arm64=\(.totals.darwin_arm64 // 0)"
       + " linux_amd64=\(.totals.linux_amd64 // 0)"
       + " checksums=\(.totals.checksums // 0)",
         "  smoke runs today (success): \(.smoke_runs_by_day[$d] // 0)"' "${OUT}"
