// @txco/op/codec — encode/decode helpers.
//
//   import { json, text } from "@txco/op/codec";

export const json = {
  parse<T = unknown>(s: string): T {
    return JSON.parse(s) as T;
  },
  stringify(v: unknown, space?: number): string {
    return JSON.stringify(v, null, space);
  },
};

const _enc = new TextEncoder();
const _dec = new TextDecoder();

export const text = {
  encode(s: string): Uint8Array {
    return _enc.encode(s);
  },
  decode(bytes: Uint8Array): string {
    return _dec.decode(bytes);
  },
};
