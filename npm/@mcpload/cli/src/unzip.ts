/**
 * Read one file out of a zip archive held in memory. Only the named entry is
 * decoded and nothing from the archive's paths is written to disk, so entry
 * names (symlinks, "../") cannot make it write outside its target. Supports the
 * stored and deflate methods, which is what `zip` produces for the release
 * archives; no zip64 (the binary is far below 4 GiB).
 */
import { inflateRawSync } from 'node:zlib';

const EOCD_SIG = 0x06054b50;
const CENTRAL_SIG = 0x02014b50;
const LOCAL_SIG = 0x04034b50;

export function readZipEntry(buf: Buffer, entry: string): Buffer {
  // End of central directory record: 22 bytes plus a comment of up to 64 KiB.
  let eocd = -1;
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 22 - 0xffff); i--) {
    if (buf.readUInt32LE(i) === EOCD_SIG) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) throw new Error('Not a zip archive: no end of central directory record');

  const count = buf.readUInt16LE(eocd + 10);
  let p = buf.readUInt32LE(eocd + 16);
  for (let n = 0; n < count; n++) {
    if (p + 46 > buf.length || buf.readUInt32LE(p) !== CENTRAL_SIG) throw new Error('Corrupt zip: bad central directory');
    const method = buf.readUInt16LE(p + 10);
    const compressedSize = buf.readUInt32LE(p + 20);
    const size = buf.readUInt32LE(p + 24);
    const nameLen = buf.readUInt16LE(p + 28);
    const local = buf.readUInt32LE(p + 42);
    const name = buf.toString('utf8', p + 46, p + 46 + nameLen);
    p += 46 + nameLen + buf.readUInt16LE(p + 30) + buf.readUInt16LE(p + 32);
    if (name !== entry) continue;

    if (local + 30 > buf.length || buf.readUInt32LE(local) !== LOCAL_SIG) throw new Error('Corrupt zip: bad local header');
    const start = local + 30 + buf.readUInt16LE(local + 26) + buf.readUInt16LE(local + 28);
    if (start + compressedSize > buf.length) throw new Error('Corrupt zip: entry runs past the end of the archive');
    const data = buf.subarray(start, start + compressedSize);
    let out: Buffer;
    if (method === 0) out = Buffer.from(data);
    else if (method === 8) out = inflateRawSync(data);
    else throw new Error(`Unsupported zip compression method ${method} for ${entry}`);
    if (out.length !== size) throw new Error(`Corrupt zip: ${entry} is ${out.length} bytes, expected ${size}`);
    return out;
  }
  throw new Error(`${entry} not found in the archive`);
}
