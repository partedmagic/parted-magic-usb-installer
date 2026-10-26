// Parted Magic USB installer (working name pmusb), macOS signing key.
//
// Copyright 2026 Patrick J. Verner, Waupaca, WI, USA
//
// This program is free software; you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation; either version 2 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details. You should have received a copy of the GNU General Public
// License along with this program; if not, see <https://www.gnu.org/licenses/>.

// make-csr makes the key pair for the Developer ID Application certificate
// in the login keychain and writes a certificate signing request (PKCS#10,
// PEM) for it, the request Keychain Access's Certificate Assistant makes.
// The private key is created inside the keychain and never leaves it.  It
// must run in the desktop session: over SSH macOS keeps the login keychain
// locked.
//
// Usage: make-csr OUT.certSigningRequest [KEYCHAIN]
//   KEYCHAIN  another keychain file instead of the default (login) one; it
//             must be unlocked already
//
// Build: swiftc -O -target arm64-apple-macos26.5 -o make-csr make-csr.swift

import Foundation
import Security

let label = "Parted Magic LLC Developer ID Application"
let commonName = "Parted Magic LLC"

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write(("make-csr: " + msg + "\n").data(using: .utf8)!)
    exit(1)
}

// DER encoding, just what a certificate request needs.
func der(_ tag: UInt8, _ content: [UInt8]) -> [UInt8] {
    var out: [UInt8] = [tag]
    let n = content.count
    if n < 0x80 {
        out.append(UInt8(n))
    } else {
        var len: [UInt8] = []
        var v = n
        while v > 0 { len.insert(UInt8(v & 0xff), at: 0); v >>= 8 }
        out.append(0x80 | UInt8(len.count))
        out += len
    }
    return out + content
}
func sequence(_ parts: [UInt8]...) -> [UInt8] { der(0x30, parts.flatMap { $0 }) }
func bitString(_ bytes: [UInt8]) -> [UInt8] { der(0x03, [0x00] + bytes) }
let null: [UInt8] = [0x05, 0x00]
let oidRSAEncryption: [UInt8] = [0x06, 0x09, 0x2A, 0x86, 0x48, 0x86, 0xF7, 0x0D, 0x01, 0x01, 0x01]
let oidSHA256WithRSA: [UInt8] = [0x06, 0x09, 0x2A, 0x86, 0x48, 0x86, 0xF7, 0x0D, 0x01, 0x01, 0x0B]
let oidCommonName: [UInt8] = [0x06, 0x03, 0x55, 0x04, 0x03]

let args = CommandLine.arguments
guard args.count == 2 || args.count == 3 else {
    fail("usage: make-csr OUT.certSigningRequest [KEYCHAIN]")
}
let outPath = args[1]
if FileManager.default.fileExists(atPath: outPath) {
    fail("\(outPath) exists already; nothing done")
}

var keychain: SecKeychain?
if args.count == 3 {
    guard SecKeychainOpen(args[2], &keychain) == errSecSuccess, keychain != nil else {
        fail("cannot open keychain \(args[2])")
    }
}

// One key of this name only: a second would leave two that look alike.
var query: [String: Any] = [
    kSecClass as String: kSecClassKey,
    kSecAttrKeyClass as String: kSecAttrKeyClassPrivate,
    kSecAttrLabel as String: label,
    kSecMatchLimit as String: kSecMatchLimitOne,
]
if let keychain { query[kSecMatchSearchList as String] = [keychain] }
if SecItemCopyMatching(query as CFDictionary, nil) == errSecSuccess {
    fail("a private key named \"\(label)\" is in the keychain already; nothing done")
}

var attrs: [String: Any] = [
    kSecAttrKeyType as String: kSecAttrKeyTypeRSA,
    kSecAttrKeySizeInBits as String: 2048,
    kSecPrivateKeyAttrs as String: [kSecAttrIsPermanent as String: true, kSecAttrLabel as String: label],
    kSecPublicKeyAttrs as String: [kSecAttrIsPermanent as String: true, kSecAttrLabel as String: label],
]
if let keychain { attrs[kSecUseKeychain as String] = keychain }
var err: Unmanaged<CFError>?
guard let priv = SecKeyCreateRandomKey(attrs as CFDictionary, &err) else {
    fail("could not make the key pair: \(err!.takeRetainedValue())")
}
guard let pub = SecKeyCopyPublicKey(priv),
      let pkcs1 = SecKeyCopyExternalRepresentation(pub, &err) as Data? else {
    fail("could not read the public key: \(err.map { "\($0.takeRetainedValue())" } ?? "none")")
}

let subject = sequence(der(0x31, sequence(oidCommonName, der(0x0C, Array(commonName.utf8)))))
let publicKeyInfo = sequence(sequence(oidRSAEncryption, null), bitString(Array(pkcs1)))
let requestInfo = sequence([0x02, 0x01, 0x00], subject, publicKeyInfo, [0xA0, 0x00])
guard let sig = SecKeyCreateSignature(priv, .rsaSignatureMessagePKCS1v15SHA256,
                                      Data(requestInfo) as CFData, &err) as Data? else {
    fail("could not sign the request: \(err!.takeRetainedValue())")
}
let request = sequence(requestInfo, sequence(oidSHA256WithRSA, null), bitString(Array(sig)))

let b64 = Data(request).base64EncodedString(options: [.lineLength64Characters, .endLineWithLineFeed])
let pem = "-----BEGIN CERTIFICATE REQUEST-----\n" + b64 + "\n-----END CERTIFICATE REQUEST-----\n"
guard FileManager.default.createFile(atPath: outPath, contents: pem.data(using: .utf8)) else {
    fail("could not write \(outPath)")
}
print("key pair \"\(label)\" made in the keychain (RSA 2048)")
print("request written: \(outPath)")
