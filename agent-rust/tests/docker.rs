use monitoring_agent::docker::decode_logs;
#[test]
fn logs_support_tty_frames_and_bounds() {
    assert_eq!(
        decode_logs("one\ntwo\n".as_bytes(), true, 1, 100).unwrap(),
        "one\n"
    );
    let mut frame = vec![1, 0, 0, 0, 0, 0, 0, 8];
    frame.extend_from_slice(b"one\ntwo\n");
    assert_eq!(decode_logs(frame.as_slice(), false, 2, 4).unwrap(), "one\n");
    frame[0] = 9;
    assert!(decode_logs(frame.as_slice(), false, 2, 100).is_err());
    assert!(decode_logs([1, 0, 0].as_slice(), false, 2, 100).is_err());
}

#[test]
fn invalid_utf8_logs_still_respect_the_byte_limit() {
    assert!(
        decode_logs([255, 255, 255, 255].as_slice(), true, 10, 4)
            .unwrap()
            .len()
            <= 4
    );
}
