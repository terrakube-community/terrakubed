import { parseApplyProgress, plannedFromChanges } from "../applyProgressParser";

describe("parseApplyProgress", () => {
  it("tracks running, done and error resources", () => {
    const log = [
      "aws_s3_bucket.a: Creating...",
      "aws_s3_bucket.a: Creation complete after 2s [id=a]",
      "aws_instance.b: Creating...",
      "aws_instance.b: Still creating... [10s elapsed]",
      "aws_db.c: Modifying...",
      "╷",
      "│ Error: creating EC2 Instance: InvalidParameter",
      "│ ",
      "│   with aws_instance.b,",
      '│   on main.tf line 4, in resource "aws_instance" "b":',
    ].join("\n");

    const { resources, errors } = parseApplyProgress(log);
    expect(resources.map((r) => [r.address, r.state])).toEqual([
      ["aws_s3_bucket.a", "done"],
      ["aws_instance.b", "error"],
      ["aws_db.c", "running"],
    ]);
    expect(errors).toEqual(["creating EC2 Instance: InvalidParameter"]);
  });

  it("treats destroy then create on one address as a replace", () => {
    const { resources } = parseApplyProgress(
      [
        "x.y: Destroying...",
        "x.y: Destruction complete after 1s",
        "x.y: Creating...",
        "x.y: Creation complete after 3s [id=2]",
      ].join("\n")
    );
    expect(resources).toHaveLength(1);
    expect(resources[0]).toMatchObject({ address: "x.y", action: "replace", state: "done", detail: "after 3s" });
  });

  it("returns nothing for empty or plan-only logs", () => {
    expect(parseApplyProgress(undefined).resources).toEqual([]);
    expect(parseApplyProgress("Refreshing state... [id=a]").resources).toEqual([]);
  });

  it("lists planned resources as pending and updates them as the log advances", () => {
    const planned = plannedFromChanges([
      { address: "a.one", actions: ["create"] },
      { address: "a.two", actions: ["update"] },
      { address: "a.three", actions: ["delete", "create"] },
      { address: "data.x.y", actions: ["read"] },
    ]);
    expect(planned.map((p) => p.address)).toEqual(["a.one", "a.two", "a.three"]);

    const log = ["a.one: Creating...", "a.one: Creation complete after 1s [id=1]", "a.two: Modifying..."].join("\n");
    const { resources } = parseApplyProgress(log, planned);
    expect(resources.map((r) => [r.address, r.state])).toEqual([
      ["a.one", "done"],
      ["a.two", "running"],
      ["a.three", "pending"],
    ]);
  });

  it("ignores data source reads", () => {
    const { resources } = parseApplyProgress(
      ["data.aws_caller_identity.current: Reading...", "data.aws_caller_identity.current: Read complete after 0s"].join(
        "\n"
      )
    );
    expect(resources).toEqual([]);
  });

  it("keeps a planned replace running until both halves complete", () => {
    const planned = plannedFromChanges([{ address: "r.x", actions: ["delete", "create"] }]);
    const half = parseApplyProgress(["r.x: Destroying...", "r.x: Destruction complete after 1s"].join("\n"), planned);
    expect(half.resources[0]).toMatchObject({ action: "replace", state: "running" });
    const full = parseApplyProgress(
      [
        "r.x: Destroying...",
        "r.x: Destruction complete after 1s",
        "r.x: Creating...",
        "r.x: Creation complete after 2s",
      ].join("\n"),
      planned
    );
    expect(full.resources[0]).toMatchObject({ action: "replace", state: "done" });
  });

  it("attaches each resource's own lines and the full error block", () => {
    const { resources } = parseApplyProgress(
      [
        "aws_instance.b: Creating...",
        "╷",
        "│ Error: creating EC2 Instance: InvalidParameter",
        "│ ",
        "│   with aws_instance.b,",
        '│   on main.tf line 4, in resource "aws_instance" "b":',
        "╵",
      ].join("\n")
    );
    expect(resources[0].state).toBe("error");
    expect(resources[0].log[0]).toBe("aws_instance.b: Creating...");
    expect(resources[0].log.join("\n")).toContain("on main.tf line 4");
  });
});
