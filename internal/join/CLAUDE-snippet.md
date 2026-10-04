## Reporting progress to sessionhub

The `sessionhub` MCP server gives you two tools. Use them so I can see what a session
is doing from the sessionhub dashboard or the herdr sidebar without opening it.

- Call `report_progress` when you finish a task, when you start a task that
  will take a while, and when you are blocked waiting on me or on something
  external. Fill `done`, `in_flight`, and `waiting_on` with short one-line
  items, and use an empty list when a category has nothing. Put anything else in
  `note`.
- Call `set_title` once the session has a clear purpose, and again if the
  purpose changes. Use a few words, for example "fix CI token rotation".

If a sessionhub tool reports that the server is unreachable, carry on with your work.
The report is queued and sent later.
