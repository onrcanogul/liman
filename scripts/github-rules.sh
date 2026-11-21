#!/usr/bin/env bash
# Applies the repository's rules on GitHub from the files in this repository,
# so they are read and reviewed like code:
#
#   - every .github/rulesets/*.json: created, or replaced if a ruleset of
#     that name exists;
#   - pull requests merge by rebase only, and their branch is deleted after;
#   - a pull request can be marked to merge itself once its checks pass
#     (gh pr merge --rebase --auto): the rules still decide when.
#
#   scripts/github-rules.sh              # the repository gh points at
#   scripts/github-rules.sh owner/name
#
# Needs gh logged in as an admin of the repository. Running it twice
# changes nothing the second time.
set -euo pipefail

cd "$(dirname "$0")/.."
repo=${1:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}

for file in .github/rulesets/*.json; do
    name=$(jq -r .name "$file")
    id=$(gh api "repos/$repo/rulesets" --paginate \
        -q ".[] | select(.name == \"$name\") | .id")
    if [ -n "$id" ]; then
        gh api --method PUT "repos/$repo/rulesets/$id" --input "$file" >/dev/null
        echo "ruleset '$name': replaced ($file)"
    else
        gh api --method POST "repos/$repo/rulesets" --input "$file" >/dev/null
        echo "ruleset '$name': created ($file)"
    fi
done

gh api --method PATCH "repos/$repo" \
    -F allow_rebase_merge=true \
    -F allow_squash_merge=false \
    -F allow_merge_commit=false \
    -F allow_update_branch=true \
    -F allow_auto_merge=true \
    -F delete_branch_on_merge=true >/dev/null
echo "pull requests: rebase only, auto-merge allowed, branch deleted after merging"
