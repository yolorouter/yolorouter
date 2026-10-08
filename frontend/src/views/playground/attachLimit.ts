// The attach guard's mirror of the backend's per-image cap, in one place.
//
// The run endpoint refuses one attachment whose data URL exceeds
// MAX_IMAGE_DATA_URL_CHARS characters (internal/handler/playground_handler.go,
// maxImageDataURLBytes — the WHOLE URL counts, the "data:image/<sub>;base64,"
// prefix included), with a 400 before the stream starts. The page refuses
// such a file locally so the console never posts a run the endpoint would
// reject; for the mirror to hold, the local byte cap must be computed from
// the encoding the file actually travels in:
//
//   FileReader.readAsDataURL renders an N-byte file as the
//   "data:image/<sub>;base64," prefix plus 4*ceil(N/3) base64 characters.
//   The prefix length is NOT uniform across the attach input's mimes
//   (PlaygroundPage.vue accept="image/png,image/jpeg,image/webp,image/gif"):
//   "data:image/png;base64," and "data:image/gif;base64," are 22 characters,
//   "data:image/jpeg;base64," and "data:image/webp;base64," are 23.
//   MAX_IMAGE_DATA_URL_PREFIX_CHARS models the LONGEST of them, so
//   dataURLChars(N) is an upper bound of the real URL length for every
//   mime the attach input accepts.
//
// So the largest file size whose URL is guaranteed to fit is
// floor((cap - prefix)/4) * 3: at N = maxAttachableImageBytes the URL is at
// most cap characters for every accepted mime, and at N+1 it already
// exceeds the cap for all of them. A plain "6 MiB" constant is NOT the
// mirror — 6 MiB of file is 8'388'630+ characters of URL (8'388'631 for
// jpeg/webp), already over the cap, and the run would die at the endpoint
// after the local guard waved the file through.
// attachLimit.test.ts pins exactly that boundary — per real mime prefix.
export const MAX_IMAGE_DATA_URL_CHARS = 8 * 1024 * 1024
export const MAX_IMAGE_DATA_URL_PREFIX_CHARS = 23
export const maxAttachableImageBytes =
  Math.floor((MAX_IMAGE_DATA_URL_CHARS - MAX_IMAGE_DATA_URL_PREFIX_CHARS) / 4) * 3

// dataURLChars is the guard's model of readAsDataURL's output length for an
// N-byte file — an upper bound across every mime the attach input accepts
// (longest prefix), kept next to the constant so the mirror stays checkable
// in one place.
export function dataURLChars(byteCount: number): number {
  return MAX_IMAGE_DATA_URL_PREFIX_CHARS + 4 * Math.ceil(byteCount / 3)
}
