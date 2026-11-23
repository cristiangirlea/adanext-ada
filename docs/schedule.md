# Scheduled jobs

Run `ada serve` to run recurring jobs on this machine. Ada checks every
30 seconds and executes one job at a time. A new job counts from creation.
After sleep or downtime, each overdue enabled job runs once. Its next interval
counts from that run. Disabling a job keeps its history and pauses execution.
Serve can run local jobs with no channel plugins enabled.

Use `every 90m` for elapsed time (minimum one minute), or five local-time cron
fields: minute (0–59), hour (0–23), day of month (1–31), month (1–12) and
day of week (0–7, Sunday is 0 or 7). Fields accept `*`, numbers, inclusive
ranges, comma-separated lists, and steps such as `*/15` or `8-18/2`.
When both day fields are restricted, either can match. With fixed hours, a
job scheduled in a skipped hour runs once when the clock jumps forward. In
a repeated hour, it runs only at the first occurrence of the scheduled time.
Wildcard or stepped hour fields follow the clock minute by minute, skipping
missing times and matching both occurrences of repeated times.

A prompt job runs as the owner in a private conversation for that job.
Channel conversations can proceed while a scheduled job is running.
The default budget is eight model calls and ten minutes. Reads can run.
A write or send that needs approval stops the job and its result names the
step the user needs to do. To schedule a prompt from chat, approve the request
showing its schedule, next three times, prompt and destination. A channel
conversation can target its own chat or keep the result local.

A script job executes the given program and arguments directly, with its
working directory at `<data_dir>/schedule/work`. Quote arguments for your
terminal. Ada supplies common system environment variables and excludes
configured provider key variables. On timeout Ada kills the process tree.
Only the local CLI can create scripts. A script runs with the OS permissions
of the account running Ada.

## Commands

    ada schedule add -name morning -spec "0 8 * * 1-5" -prompt "Summarize my open tasks" -to telegram:123
    ada schedule add -name check -spec "every 2h" -timeout 3m -script -- my-check --brief
    ada schedule list
    ada schedule disable ID
    ada schedule enable ID
    ada schedule remove ID
    ada schedule runs -n 10
    ada schedule run ID

Use `-config FILE` after `schedule` or its subcommand. Add prints the job ID.
Place Ada's options before `--`. Everything after it belongs to the script.
`run ID` runs once immediately, even for a disabled job, and prints the result
locally. It needs the same exclusive writer lock as serve or chat.

Omit `-to` for a local result. With a target, Ada sends the prompt answer or
the script's exit status and output tail only to that channel and chat.
Long answers are split. A missing channel is reported on stderr. Run history
records whether Ada received confirmation for every part. If the wait times
out or the channel disconnects before confirmation, the run is recorded as
undelivered. The channel ledger may still send the pending parts later.

Jobs are stored in `<data_dir>/schedule/jobs.json`. Each completed run appends
a row to `runs.jsonl` in the same directory with start and finish times,
status, delivery confirmation and up to 64 KiB of output. Removing a job
keeps its run history.
