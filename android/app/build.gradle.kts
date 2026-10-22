plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

// The release key and its password stay out of the source: build-android.sh
// passes them in from the environment (PM_KEYSTORE, PM_KEYSTORE_PASSWORD).
val pmKeystore: String? = System.getenv("PM_KEYSTORE")
val pmKeystorePassword: String? = System.getenv("PM_KEYSTORE_PASSWORD")

android {
    namespace = "com.partedmagic.usbinstaller"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.partedmagic.usbinstaller"
        minSdk = 26
        targetSdk = 36
        versionCode = 2
        versionName = "0.2"
        // The pmusb core is built for arm64 only (every phone with USB-C).
        ndk { abiFilters += listOf("arm64-v8a") }
    }

    signingConfigs {
        if (pmKeystore != null && pmKeystorePassword != null) {
            create("pm") {
                storeFile = file(pmKeystore)
                storePassword = pmKeystorePassword
                keyAlias = "partedmagic"
                keyPassword = pmKeystorePassword
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            signingConfigs.findByName("pm")?.let { signingConfig = it }
        }
    }
    // The core is a program, not a library: it ships as lib/arm64-v8a/
    // libpmusb.so so Android puts it in nativeLibraryDir, the one place an
    // app may run a program from. It must be extracted to disk to run.
    packaging {
        jniLibs { useLegacyPackaging = true }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    buildFeatures {
        compose = true
    }
}

dependencies {
    implementation(platform("androidx.compose:compose-bom:2024.10.01"))
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.core:core-ktx:1.13.1")
}
