#!/usr/bin/env bash
# #1165, ADR 0090: keeps one open issue per weekly finding, found by its
# exact title.
#
#   rolling-issue.sh open TITLE BODY_FILE   open it, or replace its body
#   rolling-issue.sh close TITLE            close it, if open
#
# Needs GH_TOKEN with issues: write, and RUN, the run's URL.
set -euo pipefail

action=$1 title=$2
number=$(gh issue list --state open --search "in:title \"$title\"" --json number,title \
  --jq "map(select(.title == \"$title\"))[0].number // empty")
case "$action" in
  open)
    if [ -n "$number" ]; then
      gh issue edit "$number" --body-file "$3"
      gh issue comment "$number" --body "Updated by $RUN."
    else
      gh issue create --title "$title" --body-file "$3" --label needs-triage
    fi
    ;;
  close)
    if [ -n "$number" ]; then
      gh issue close "$number" --comment "Nothing left to report as of $RUN."
    fi
    ;;
  *) echo "usage: rolling-issue.sh open TITLE BODY_FILE | close TITLE" >&2; exit 2 ;;
esac
