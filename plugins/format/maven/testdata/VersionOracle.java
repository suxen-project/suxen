// Generates direct-pair signs from Apache Maven ComparableVersion 3.9.16.
// Compile and run with Java 21 and maven-artifact-3.9.16.jar; normal Go tests
// read the generated matrix and do not need either tool.
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import org.apache.maven.artifact.versioning.ComparableVersion;

public final class VersionOracle {
    public static void main(String[] arguments) throws Exception {
        if (arguments.length != 1) {
            throw new IllegalArgumentException("usage: VersionOracle VERSIONS_FILE");
        }
        var versions = Files.readAllLines(Path.of(arguments[0]), StandardCharsets.UTF_8);
        var parsed = versions.stream().map(ComparableVersion::new).toList();
        for (var left : parsed) {
            var row = new StringBuilder(parsed.size());
            for (var right : parsed) {
                // '0', '1', '2' represent negative, zero, and positive.
                row.append((char) ('1' + Integer.signum(left.compareTo(right))));
            }
            System.out.println(row);
        }
    }
}
