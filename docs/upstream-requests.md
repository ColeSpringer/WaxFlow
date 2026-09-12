# Upstream requests

The standing list of things WaxFlow wants from the sibling Wax repos it
depends on. Only wax-series dependencies belong here; today that is
WaxLabel alone (`cli/go.mod` and `oracletest/go.mod`; the root module's
require block is empty by design, see ADR-0002). Every entry is a
candidate for whenever upstream work is next scheduled; nothing here
implies timing, and none of it is a WaxFlow prerequisite. Each entry
names the shipped workaround WaxFlow runs on today and, where one exists,
the test that will notice the upstream fix landing. Agents: when you
defer something because it needs upstream support, add it here in the
same change; do not bury it in a progress note.

## WaxLabel

No open requests.
