const encoder = new TextEncoder();

export function sampleContent(file, title) {
  return encoder.encode(`FileMind demo\n\nTransfer: ${title}\nFile: ${file.name}\n\nThis is generated sample content. No original files are uploaded or shared in this demo.\n`);
}

function crc32(data) {
  let crc = 0xffffffff;
  for (const byte of data) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

// ZIP entries contain generated text, never the visitor's selected files.
export function sampleArchive(files, title) {
  const parts = [], directory = [];
  let offset = 0, directorySize = 0;
  for (const [index, file] of files.entries()) {
    const safeName = file.name.replace(/[\\/\u0000-\u001f]/g, '_');
    const name = encoder.encode(`${index + 1}-${safeName}.demo.txt`);
    const data = sampleContent(file, title), checksum = crc32(data);
    const header = new Uint8Array(30 + name.length), view = new DataView(header.buffer);
    view.setUint32(0, 0x04034b50, true);
    view.setUint16(4, 20, true);
    view.setUint16(6, 0x800, true);
    view.setUint16(12, 33, true); // January 1, 1980.
    view.setUint32(14, checksum, true);
    view.setUint32(18, data.length, true);
    view.setUint32(22, data.length, true);
    view.setUint16(26, name.length, true);
    header.set(name, 30);
    const entry = new Uint8Array(46 + name.length), central = new DataView(entry.buffer);
    central.setUint32(0, 0x02014b50, true);
    central.setUint16(4, 20, true);
    central.setUint16(6, 20, true);
    central.setUint16(8, 0x800, true);
    central.setUint16(14, 33, true);
    central.setUint32(16, checksum, true);
    central.setUint32(20, data.length, true);
    central.setUint32(24, data.length, true);
    central.setUint16(28, name.length, true);
    central.setUint32(42, offset, true);
    entry.set(name, 46);
    parts.push(header, data);
    directory.push(entry);
    offset += header.length + data.length;
    directorySize += entry.length;
  }
  const end = new Uint8Array(22), view = new DataView(end.buffer);
  view.setUint32(0, 0x06054b50, true);
  view.setUint16(8, files.length, true);
  view.setUint16(10, files.length, true);
  view.setUint32(12, directorySize, true);
  view.setUint32(16, offset, true);
  return new Blob([...parts, ...directory, end], { type: 'application/zip' });
}

export function download(blob, name) {
  const url = URL.createObjectURL(blob), link = document.createElement('a');
  link.href = url;
  link.download = name.replace(/[\\/\u0000-\u001f]/g, '_');
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
