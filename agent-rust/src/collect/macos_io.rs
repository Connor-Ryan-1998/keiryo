//! Device-level IOKit counters, independent of mounted volumes.
use crate::Result;
use anyhow::ensure;
use std::{
    ffi::{CStr, c_char, c_void},
    ptr,
};

#[link(name = "IOKit", kind = "framework")]
unsafe extern "C" {
    fn IOServiceMatching(name: *const c_char) -> *const c_void;
    fn IOServiceGetMatchingServices(port: u32, matching: *const c_void, iterator: *mut u32) -> i32;
    fn IOIteratorNext(iterator: u32) -> u32;
    fn IOObjectRelease(object: u32) -> i32;
    fn IOObjectConformsTo(object: u32, class: *const c_char) -> u32;
    fn IORegistryEntryGetParentEntry(object: u32, plane: *const c_char, parent: *mut u32) -> i32;
    fn IORegistryEntryCreateCFProperty(
        object: u32,
        key: *const c_void,
        allocator: *const c_void,
        options: u32,
    ) -> *const c_void;
}
#[link(name = "CoreFoundation", kind = "framework")]
unsafe extern "C" {
    static kCFBooleanTrue: *const c_void;
    fn CFRelease(value: *const c_void);
    fn CFStringCreateWithCString(
        allocator: *const c_void,
        value: *const c_char,
        encoding: u32,
    ) -> *const c_void;
    fn CFStringGetCString(
        value: *const c_void,
        buffer: *mut c_char,
        size: isize,
        encoding: u32,
    ) -> u8;
    fn CFDictionarySetValue(dictionary: *const c_void, key: *const c_void, value: *const c_void);
    fn CFDictionaryGetValue(dictionary: *const c_void, key: *const c_void) -> *const c_void;
    fn CFGetTypeID(value: *const c_void) -> usize;
    fn CFDictionaryGetTypeID() -> usize;
    fn CFStringGetTypeID() -> usize;
    fn CFNumberGetTypeID() -> usize;
    fn CFNumberGetValue(number: *const c_void, kind: isize, value: *mut i64) -> u8;
}
const UTF8: u32 = 0x08000100;
struct Cf(*const c_void);
impl Drop for Cf {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: Cf owns one retained CF object, released exactly once here.
            unsafe { CFRelease(self.0) }
        }
    }
}
struct Io(u32);
impl Drop for Io {
    fn drop(&mut self) {
        if self.0 != 0 {
            // SAFETY: Io owns one live IOKit reference, released exactly once here.
            unsafe { IOObjectRelease(self.0) };
        }
    }
}
fn key(text: &CStr) -> Result<Cf> {
    // SAFETY: text is NUL-terminated; null selects the default allocator.
    let value = Cf(unsafe { CFStringCreateWithCString(ptr::null(), text.as_ptr(), UTF8) });
    ensure!(!value.0.is_null(), "IOKit key unavailable");
    Ok(value)
}
fn property(object: u32, name: &CStr) -> Result<Cf> {
    let key = key(name)?;
    // SAFETY: callers hold object alive; key is retained and null selects the allocator.
    let value = Cf(unsafe { IORegistryEntryCreateCFProperty(object, key.0, ptr::null(), 0) });
    ensure!(!value.0.is_null(), "IOKit property unavailable");
    Ok(value)
}
fn number(dictionary: *const c_void, name: &CStr) -> Result<u64> {
    let key = key(name)?;
    // SAFETY: caller retains dictionary; null/type checks guard CF reads, and kind 4
    // requests an i64 into aligned storage. Dictionary retains the borrowed number.
    unsafe {
        ensure!(
            !dictionary.is_null() && CFGetTypeID(dictionary) == CFDictionaryGetTypeID(),
            "invalid IOKit statistics"
        );
        let value = CFDictionaryGetValue(dictionary, key.0);
        ensure!(
            !value.is_null() && CFGetTypeID(value) == CFNumberGetTypeID(),
            "missing IOKit counter"
        );
        let mut result = 0i64;
        ensure!(
            CFNumberGetValue(value, 4, &mut result) != 0 && result >= 0,
            "invalid IOKit counter"
        );
        Ok(result as u64)
    }
}
pub(super) fn collect() -> Result<Vec<(String, u64, u64)>> {
    // SAFETY: class name is NUL-terminated; result is a retained mutable dictionary.
    let matching = Cf(unsafe { IOServiceMatching(c"IOMedia".as_ptr()) });
    ensure!(!matching.0.is_null(), "IOKit matching unavailable");
    let whole = key(c"Whole")?;
    // SAFETY: dictionary/key are retained; kCFBooleanTrue is an immortal CF object.
    unsafe { CFDictionarySetValue(matching.0, whole.0, kCFBooleanTrue) };
    let mut iterator = 0;
    let pointer = matching.0;
    std::mem::forget(matching);
    // SAFETY: consumes the retained dictionary even on failure; iterator is writable.
    let status = unsafe { IOServiceGetMatchingServices(0, pointer, &mut iterator) };
    ensure!(status == 0, "IOKit enumeration failed");
    let iterator = Io(iterator);
    let mut result = Vec::new();
    loop {
        // SAFETY: iterator owns a live iterator; each nonzero result is retained.
        let media = Io(unsafe { IOIteratorNext(iterator.0) });
        if media.0 == 0 {
            break;
        }
        let mut parent = 0;
        // SAFETY: media is retained, plane is NUL-terminated, and parent is writable.
        let status =
            unsafe { IORegistryEntryGetParentEntry(media.0, c"IOService".as_ptr(), &mut parent) };
        ensure!(status == 0, "IOKit parent lookup failed");
        let parent = Io(parent);
        // SAFETY: parent is retained and class name is NUL-terminated.
        if unsafe { IOObjectConformsTo(parent.0, c"IOBlockStorageDriver".as_ptr()) } == 0 {
            continue;
        }
        let name = property(media.0, c"BSD Name")?;
        // SAFETY: name owns a nonnull CF object; type IDs require no arguments.
        ensure!(
            unsafe { CFGetTypeID(name.0) == CFStringGetTypeID() },
            "invalid disk identity"
        );
        let mut buffer = [0i8; 256];
        // SAFETY: name is a retained CFString; buffer is writable for the given length.
        let converted =
            unsafe { CFStringGetCString(name.0, buffer.as_mut_ptr(), buffer.len() as isize, UTF8) };
        ensure!(converted != 0, "invalid disk identity");
        // SAFETY: successful conversion wrote a NUL-terminated string inside buffer.
        let device = unsafe { CStr::from_ptr(buffer.as_ptr()) }
            .to_str()?
            .to_string();
        let stats = property(parent.0, c"Statistics")?;
        result.push((
            device,
            number(stats.0, c"Bytes (Read)")?,
            number(stats.0, c"Bytes (Write)")?,
        ));
    }
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unavailable_counter_is_not_zero() {
        assert!(number(ptr::null(), c"Bytes (Read)").is_err());
    }
}
