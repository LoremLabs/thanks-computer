// @txco/op/schema — a tiny runtime validator with a zod-flavored surface.
// Covers the common primitives; not a full zod. Throws on invalid input.
//
//   import { z } from "@txco/op/schema";
//   const Email = z.object({ to: z.string(), cc: z.array(z.string()).optional() });
//   const v = Email.parse(input.email);

export class SchemaError extends Error {
  constructor(public path: string, message: string) {
    super(path ? `${path}: ${message}` : message);
    this.name = "SchemaError";
  }
}

export abstract class Schema<T> {
  abstract check(value: unknown, path: string): T;

  parse(value: unknown): T {
    return this.check(value, "");
  }

  optional(): Schema<T | undefined> {
    return new OptionalSchema<T>(this);
  }
}

class OptionalSchema<T> extends Schema<T | undefined> {
  constructor(private inner: Schema<T>) {
    super();
  }
  check(value: unknown, path: string): T | undefined {
    if (value === undefined) return undefined;
    return this.inner.check(value, path);
  }
}

class StringSchema extends Schema<string> {
  check(value: unknown, path: string): string {
    if (typeof value !== "string") throw new SchemaError(path, `expected string, got ${typeof value}`);
    return value;
  }
}

class NumberSchema extends Schema<number> {
  check(value: unknown, path: string): number {
    if (typeof value !== "number" || Number.isNaN(value))
      throw new SchemaError(path, `expected number, got ${typeof value}`);
    return value;
  }
}

class BooleanSchema extends Schema<boolean> {
  check(value: unknown, path: string): boolean {
    if (typeof value !== "boolean") throw new SchemaError(path, `expected boolean, got ${typeof value}`);
    return value;
  }
}

class ArraySchema<T> extends Schema<T[]> {
  constructor(private item: Schema<T>) {
    super();
  }
  check(value: unknown, path: string): T[] {
    if (!Array.isArray(value)) throw new SchemaError(path, `expected array, got ${typeof value}`);
    return value.map((v, i) => this.item.check(v, `${path}[${i}]`));
  }
}

type Shape = Record<string, Schema<unknown>>;
type Infer<S extends Shape> = { [K in keyof S]: S[K] extends Schema<infer U> ? U : never };

class ObjectSchema<S extends Shape> extends Schema<Infer<S>> {
  constructor(private shape: S) {
    super();
  }
  check(value: unknown, path: string): Infer<S> {
    if (value === null || typeof value !== "object" || Array.isArray(value))
      throw new SchemaError(path, `expected object, got ${value === null ? "null" : typeof value}`);
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(this.shape)) {
      const sub = path ? `${path}.${key}` : key;
      out[key] = this.shape[key].check((value as Record<string, unknown>)[key], sub);
    }
    return out as Infer<S>;
  }
}

export const z = {
  string: () => new StringSchema(),
  number: () => new NumberSchema(),
  boolean: () => new BooleanSchema(),
  array: <T>(item: Schema<T>) => new ArraySchema<T>(item),
  object: <S extends Shape>(shape: S) => new ObjectSchema<S>(shape),
};
