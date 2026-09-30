// Independent JCS reference (RFC 8785, Appendix A style) used by the Ruby
// specs to cross-check canonical output against a different implementation.
// Input is read from stdin as a JSON array of IEEE 754 hex bit patterns;
// output is a JSON array of ECMAScript number serializations.

const fs = require("fs");

const bits = JSON.parse(fs.readFileSync(0, "utf8"));
const buffer = Buffer.alloc(8);

const numbers = bits.map((hex) => {
  buffer.write(hex, 0, "hex");
  return buffer.readDoubleBE(0);
});

const canonical = (value) => {
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error("NaN and Infinity are not valid JSON numbers");
    }
    return Object.is(value, -0) ? "0" : String(value);
  }
  if (value === null) return "null";
  if (typeof value === "boolean") return String(value);
  if (typeof value === "string") return JSON.stringify(value);
  if (Array.isArray(value)) {
    return "[" + value.map(canonical).join(",") + "]";
  }
  const keys = Object.keys(value).sort();
  return (
    "{" +
    keys
      .map((key) => JSON.stringify(key) + ":" + canonical(value[key]))
      .join(",") +
    "}"
  );
};

process.stdout.write(JSON.stringify(numbers.map(canonical)));
