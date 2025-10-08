### emit_log Serialization Decoding Guide

This document specifies how to decode the `Args` byte sequence that accompanies an `emit_log` hostio event in Stylus traces (as surfaced via `debug_traceCall`).

#### Byte Sequence Structure

```
Offset  Size        Type (encoded)        Meaning
0       4 bytes     u32 (big-endian)      Topic count `topicCount`
4       32 * n      topic[n] (bytes32)    n 32-byte topic hashes, contiguous
rest    m bytes     bytes (raw)           Log data payload
```

`n` equals `topicCount`. `m` may be zero. No additional padding or length prefixes appear after the header.

#### Decoding Procedure

1. **Read Topic Count (`u32`)**
   - Bytes `[0..4)` form a big-endian unsigned 32-bit integer.
   - Conventionally limited to the range `0…4`; higher values indicate malformed input.

2. **Extract Topics (`topicCount × bytes32`)**
   - For each index `i` in `0…topicCount-1`, slice bytes `[4 + 32*i … 4 + 32*(i+1))`.
   - Treat each slice as an EVM topic hash (32-byte word).
   - Preserve the original order; `topic[0]` corresponds to `LOGn`’s `topic0`, etc.

3. **Extract Log Data (`bytes`)**
   - Remaining bytes starting at offset `4 + 32*topicCount` form the log payload.
   - Interpret as arbitrary byte array (may be empty).
   - No alignment or padding: payload length is `totalLen - 4 - 32*topicCount`.

#### Mapping to EVM Event ABI

- `topic[0]` (if `topicCount > 0`) is usually the Keccak-256 hash of the event signature (e.g., `keccak256("Transfer(address,address,uint256)")`).
- Additional topics represent indexed event parameters encoded as 32-byte ABI words.
- The data payload contains the concatenated ABI encoding of all non-indexed event parameters, identical to Ethereum’s event ABI rules. You must know the event signature to decode individual fields.

#### ABI Decoding Cheat Sheet (Indexed Parameters → Topics)

- **`uint<M>` / `int<M>` / `bool`**: 32-byte word; decode exactly as you would pop from the stack (big-endian, two’s complement for signed).
- **`address`**: rightmost 20 bytes of the 32-byte topic; leftmost 12 bytes are zero padding.
- **`bytes32` / `hash`**: topic already is the 32-byte value.
- **Static tuple or fixed-size array**: not supported directly; the entire tuple serializes to its Keccak-256 hash before being placed in a topic.
- **Dynamic types (string, bytes, dynamic arrays, dynamic tuples, structs)**: Solidity ABI stores `keccak256(value)` in the topic. You must use the original event parameters or compare hashes because the raw value is not present.

#### ABI Decoding Cheat Sheet (Non-Indexed Parameters → Data Payload)

Static types occupy fixed 32-byte slots; dynamic types use 32-byte offsets into a tail section. The following summary matches the Ethereum ABI specification (`soliditylang.org/docs/abi-spec.html`):

- **`uint<M>` / `int<M>`**: big-endian two’s-complement in 32 bytes; `<M>` must be ≤256 and divisible by 8. Example: decode by reading the 32-byte word as unsigned/signed integer.
- **`bool`**: same as `uint8`; value is `0` or `1`.
- **`address`**: rightmost 20 bytes contain the address; leftmost 12 bytes are zero padding.
- **`bytes32` / `keccak256` hashes**: 32 raw bytes.
- **Static `tuple` / fixed-size array (`T[k]`)**: concatenation of each element’s 32-byte encoding.
- **`bytes` / `string` / dynamic array**:
  - Word `w0`: 32-byte offset (from start of the data section) to the actual payload.
  - At `offset`: 32-byte length `L`.
  - Followed by `ceil(L/32)` words of data (right-padded with zeros).
- **Dynamic tuple**: treat each component like a standalone field; static components appear inline, dynamic components hold offsets into the shared tail.

Decoding recipe for payload:
1. Split the payload into 32-byte words.
2. For each parameter (in declaration order) apply the ABI rules:
   - Static parameter: interpret its word(s) directly.
   - Dynamic parameter: read its offset word, jump to `payloadStart + offset`, read length and the subsequent bytes.
3. Apply type-specific conversions (e.g., trim leading zeros for addresses and shorter ints, decode UTF-8 for strings, iterate arrays).

Remember: Stylus does not prepend additional metadata—ABI semantics are identical to standard Ethereum logs. Use the event signature or ABI supplied by the contract to decide which decoding path to follow.
