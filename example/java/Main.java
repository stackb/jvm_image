package example.java;

import com.google.common.collect.ImmutableList;

public class Main {
    public static void main(String[] args) {
        ImmutableList<String> greetings = ImmutableList.of("Hello", "world!");
        System.out.println(String.join(", ", greetings));
    }
}
