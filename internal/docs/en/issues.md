# Issues

The "Issues" section lists errors grouped by fingerprint — for each one you see its level, 24-hour trend, event count, status, and assignee.

## List and filters
The table can be sorted by last seen, first seen, or frequency. Filters narrow the list by status (unresolved/resolved/ignored), level (debug…fatal), environment, time period, and a search query over the title or culprit.

## How grouping works
Every incoming event gets a fingerprint — a key computed from the error type, message, and code location. New events sharing the same fingerprint land in the existing issue and bump its "Count"; a fingerprint seen for the first time creates a new issue.

## Issue detail
At the top is the stack trace of the most recent event; below is a list of recent events you can switch between. Each event exposes tags, user data, SDK info, and arbitrary contexts, plus a link to the related trace in "Performance".

## Actions
An issue can be resolved, ignored, or returned to unresolved, and assigned to any member of the project.
