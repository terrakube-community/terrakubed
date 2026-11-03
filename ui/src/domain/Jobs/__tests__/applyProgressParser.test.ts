import { parseApplyProgress } from "../applyProgressParser";

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
    expect(resources).toEqual([{ address: "x.y", action: "replace", state: "done", detail: "after 3s" }]);
  });

  it("returns nothing for empty or plan-only logs", () => {
    expect(parseApplyProgress(undefined).resources).toEqual([]);
    expect(parseApplyProgress("Refreshing state... [id=a]").resources).toEqual([]);
  });
});
