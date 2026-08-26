import * as sandcastle from "@ai-hero/sandcastle";
import { podman } from "@ai-hero/sandcastle/sandboxes/podman";

const MAX_ITERATIONS = 40;

// Concurrent issue sandboxes. obsync's ticket graph (#22–#44) is close to a
// chain — most slices are blocked by the one before them — so in practice the
// planner returns one or two issues and this ceiling rarely binds. It still
// matters as a bound on nested-container pressure: seam 2 has agents running
// `docker build` and `docker run` through the shared Podman socket, and
// Podman's provider exposes no memory ceiling, so if image builds start dying
// mid-run this number is the lever.
const MAX_PARALLEL = 3;

// Every sandbox gets the same mounts, so the planner, the per-issue implementers
// and the merger all run against an identical environment.
const MOUNTS = [
  {
    hostPath: "/home/andy/.claude/.credentials.json",
    sandboxPath: "~/.claude/.credentials.json",
  },
  // Docker-outside-of-Docker. The Containerfile installs the Docker CLI, Buildx
  // and Compose but no daemon. obsync's second test seam *is* the container
  // image — build it, run it at `--user 4242:4242` against a throwaway vault
  // and a `file://` bare remote, and assert the clone/commit/push plus a
  // healthy `docker inspect`. That seam is the only thing checking the
  // no-`/etc/passwd` claim, the git-floor gate, the `HEALTHCHECK` wiring and
  // the credential re-read, and it cannot run without a real daemon. The
  // reference `compose.yaml` is exercised the same way, because it is
  // normative: a normative file nobody runs is an example with a stronger
  // adjective.
  //
  // Rootless Podman serves a Docker-compatible API, and its socket is owned by
  // the host user, so `--userns=keep-id` maps it to `agent` inside the
  // container — no `--group-add` needed, which matters because runc (this
  // host's OCI runtime) rejects `keep-groups`. Mounting it at the default
  // socket path means no DOCKER_HOST is required either.
  //
  // Prerequisite: `systemctl --user enable --now podman.socket`. Without it this
  // hostPath is missing and sandbox creation fails before any agent starts.
  {
    hostPath: `/run/user/${process.getuid!()}/podman/podman.sock`,
    sandboxPath: "/var/run/docker.sock",
  },
];

// Warm the Go caches before the agent's clock starts. Both hooks are
// deliberately tolerant of an absent `go.mod`: slice 01 (#22) is the ticket
// that creates it, so on the first iterations there is nothing to download and
// a hard failure here would fail sandbox creation and the agent would never
// run. After that, a stable branch name (see plan-prompt.md) keeps the worktree
// and its `GOMODCACHE`/`GOCACHE` around, so these become near-no-ops.
//
// There is no npm and no uv in this image — obsync is a single Go module plus
// its container image, and nothing else.
const INSTALL_HOOKS = {
  host: {
    onSandboxReady: [
      {
        command: "test -f go.mod && go mod download || echo 'no go.mod yet'",
        timeoutMs: 600_000,
      },
      {
        command: "test -f go.mod && go build ./... || echo 'no go.mod yet'",
        timeoutMs: 600_000,
      },
    ],
  },
} as const;

for (let iteration = 1; iteration <= MAX_ITERATIONS; iteration++) {
  console.log(`\n=== Iteration ${iteration}/${MAX_ITERATIONS} ===\n`);

  // Phase 1: Plan — orchestrator agent analyzes issues and picks parallelizable work
  const plan = await sandcastle.run({
    sandbox: podman({ mounts: MOUNTS }),
    name: "Planner",
    agent: sandcastle.claudeCode("claude-opus-5"),
    promptFile: "./.sandcastle/plan-prompt.md",
  });

  const planMatch = plan.stdout.match(/<plan>([\s\S]*?)<\/plan>/);
  if (!planMatch) {
    throw new Error(
      "Orchestrator did not produce a <plan> tag.\n\n" + plan.stdout
    );
  }

  const { issues } = JSON.parse(planMatch[1]) as {
    issues: { number: number; title: string; branch: string }[];
  };

  if (issues.length === 0) {
    console.log("No issues to work on. Exiting.");
    break;
  }

  console.log(
    `Planning complete. ${issues.length} issue(s) to work in parallel:`
  );
  for (const issue of issues) {
    console.log(`  #${issue.number}: ${issue.title} → ${issue.branch}`);
  }

  // Phase 2: Execute + Review — implement then review each branch
  let running = 0;
  const queue: (() => void)[] = [];
  const acquire = () =>
    running < MAX_PARALLEL
      ? (running++, Promise.resolve())
      : new Promise<void>((resolve) => queue.push(resolve));
  const release = () => {
    running--;
    const next = queue.shift();
    if (next) {
      running++;
      next();
    }
  };

  const settled = await Promise.allSettled(
    issues.map(async (issue) => {
      await acquire();
      try {
        await using sandbox = await sandcastle.createSandbox({
          sandbox: podman({ mounts: MOUNTS }),
          branch: issue.branch,
          hooks: INSTALL_HOOKS,
        });

        const result = await sandbox.run({
          name: "Implementer #" + issue.number,
          agent: sandcastle.claudeCode("claude-opus-5"),
          promptFile: "./.sandcastle/implement-prompt.md",
          promptArgs: {
            ISSUE_NUMBER: String(issue.number),
            ISSUE_TITLE: issue.title,
            BRANCH: issue.branch,
          },
        });

        if (result.commits.length > 0) {
          await sandbox.run({
            name: "Reviewer #" + issue.number,
            agent: sandcastle.claudeCode("claude-opus-5"),
            promptFile: "./.sandcastle/review-prompt.md",
            promptArgs: {
              ISSUE_NUMBER: String(issue.number),
              ISSUE_TITLE: issue.title,
              BRANCH: issue.branch,
            },
          });
        }

        return result;
      } finally {
        release();
      }
    })
  );

  for (const [i, outcome] of settled.entries()) {
    if (outcome.status === "rejected") {
      console.error(
        `  ✗ #${issues[i].number} (${issues[i].branch}) failed: ${outcome.reason}`
      );
    }
  }

  const completedIssues = settled
    .map((outcome, i) => ({ outcome, issue: issues[i] }))
    .filter(
      (
        entry
      ): entry is {
        outcome: PromiseFulfilledResult<
          Awaited<ReturnType<typeof sandcastle.run>>
        >;
        issue: (typeof issues)[number];
      } =>
        entry.outcome.status === "fulfilled" &&
        entry.outcome.value.commits.length > 0
    )
    .map((entry) => entry.issue);

  const completedBranches = completedIssues.map((i) => i.branch);

  console.log(
    `\nExecution complete. ${completedBranches.length} branch(es) with commits:`
  );
  for (const branch of completedBranches) {
    console.log(`  ${branch}`);
  }

  if (completedBranches.length === 0) {
    console.log("No commits produced. Nothing to merge.");
    continue;
  }

  // Phase 3: Merge — one agent merges all branches together. It runs the same
  // full check suite the implementers do (see merge-prompt.md), so it needs the
  // same warm caches; without them a cold `go mod download` comes out of the
  // merger's clock before it can tell a merge failure from a missing module.
  await sandcastle.run({
    sandbox: podman({ mounts: MOUNTS }),
    name: "Merger",
    maxIterations: 10,
    hooks: INSTALL_HOOKS,
    agent: sandcastle.claudeCode("claude-opus-5"),
    promptFile: "./.sandcastle/merge-prompt.md",
    promptArgs: {
      BRANCHES: completedBranches.map((b) => `- ${b}`).join("\n"),
      ISSUES: completedIssues
        .map((i) => `- #${i.number}: ${i.title}`)
        .join("\n"),
    },
  });

  console.log("\nBranches merged.");
}

console.log("\nAll done.");
