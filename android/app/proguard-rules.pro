# Parted Magic USB Installer: R8 keep rules.
#
# None are needed beyond the defaults: MainActivity is declared in the
# manifest, JSON is read by hand with org.json from literal keys, and there
# is no reflection, Serializable, Parcelable or JNI. The pmusb core is a
# separate program run as a child process, not a library R8 sees.
