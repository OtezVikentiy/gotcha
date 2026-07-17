# Profiling

A profile is a snapshot of where CPU time or execution time goes inside your application, captured by the SDK during real requests.

## Flame graph
A flame graph visualizes a profile as a stack of call frames: a block's width corresponds to the share of time the function took, and nesting corresponds to call depth. It's the fastest way to spot the "heaviest" execution branch.

## In-app vs. system frames
Frames are split into your application's own code (in-app) and runtime/standard-library code (system). By default the focus is on in-app frames — that's usually where the optimization target lives.

## Relation to transactions and regressions
Profiles are linked to transactions from "Performance", so you can jump straight from a slow request to its flame graph. Just like with latency, profiles are tracked for regressions — a significant increase in time spent in a specific function relative to a baseline.
