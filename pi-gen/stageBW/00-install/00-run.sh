#!/bin/bash -e
install -v -m 0755 files/bordwaechter             "${ROOTFS_DIR}/usr/local/bin/bordwaechter"
install -v -m 0755 files/bordwaechter-firstboot.sh "${ROOTFS_DIR}/usr/local/bin/bordwaechter-firstboot.sh"
install -v -m 0644 files/bordwaechter.service      "${ROOTFS_DIR}/etc/systemd/system/bordwaechter.service"
install -v -m 0644 files/bordwaechter-firstboot.service "${ROOTFS_DIR}/etc/systemd/system/bordwaechter-firstboot.service"
install -v -d "${ROOTFS_DIR}/etc/bordwaechter"
install -v -m 0600 files/config.json              "${ROOTFS_DIR}/etc/bordwaechter/config.json"
ln -sf /usr/share/zoneinfo/Europe/Berlin "${ROOTFS_DIR}/etc/localtime"
echo "Europe/Berlin" > "${ROOTFS_DIR}/etc/timezone"
